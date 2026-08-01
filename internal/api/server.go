// Package api exposes the local HTTP API and the embedded management UI.
// It binds to loopback only. Handlers never touch printers directly: every
// print path goes through the job service and the printer manager.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"print-agent/internal/bluetooth"
	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/printers"
	"print-agent/internal/webui"
)

type Server struct {
	jobsService *jobs.Service
	jobsRepo    *jobs.Repository
	manager     *printers.Manager
	configRepo  *config.Repository
	connector   bluetooth.Connector
	diag        *diagnostics.Service
	auth        *AuthService
	bus         *events.Bus
	limiter     *rateLimiter
	log         *slog.Logger
}

func NewServer(jobsService *jobs.Service, jobsRepo *jobs.Repository, manager *printers.Manager,
	configRepo *config.Repository, connector bluetooth.Connector, diag *diagnostics.Service,
	auth *AuthService, bus *events.Bus, log *slog.Logger) *Server {
	return &Server{
		jobsService: jobsService,
		jobsRepo:    jobsRepo,
		manager:     manager,
		configRepo:  configRepo,
		connector:   connector,
		diag:        diag,
		auth:        auth,
		bus:         bus,
		limiter:     newRateLimiter(20, 40),
		log:         log,
	}
}

// Handler builds the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// POS-facing endpoints: CORS + auth + rate/body limits.
	pos := func(h http.HandlerFunc) http.Handler {
		return chain(h, s.posCORS, s.rateLimit, s.limitBody, s.requireAuth)
	}
	mux.Handle("POST /api/v1/jobs", pos(s.handleCreateJob))
	mux.Handle("OPTIONS /api/v1/jobs", pos(s.noContent))
	mux.Handle("GET /api/v1/jobs/{jobID}", pos(s.handleGetJob))
	mux.Handle("OPTIONS /api/v1/jobs/{jobID}", pos(s.noContent))
	mux.Handle("GET /api/v1/status", pos(s.handleStatus))
	mux.Handle("OPTIONS /api/v1/status", pos(s.noContent))
	mux.Handle("POST /api/v1/pair", chain(http.HandlerFunc(s.handlePair), s.posCORS, s.rateLimit, s.limitBody))
	mux.Handle("OPTIONS /api/v1/pair", chain(http.HandlerFunc(s.noContent), s.posCORS))

	// Health is unauthenticated and origin-agnostic by design.
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	// Management endpoints: local browser or local tools only.
	admin := func(h http.HandlerFunc) http.Handler {
		return chain(h, s.localOnly, s.limitBody)
	}
	mux.Handle("GET /api/v1/printers", admin(s.handleListPrinters))
	mux.Handle("POST /api/v1/printers", admin(s.handleCreatePrinter))
	mux.Handle("PUT /api/v1/printers/{printerID}", admin(s.handleUpdatePrinter))
	mux.Handle("DELETE /api/v1/printers/{printerID}", admin(s.handleDeletePrinter))
	mux.Handle("POST /api/v1/printers/reconnect-all", admin(s.handleReconnectAll))
	mux.Handle("POST /api/v1/printers/{printerID}/reconnect", admin(s.handleReconnect))
	mux.Handle("POST /api/v1/printers/{printerID}/test", admin(s.handleTestPrint))
	mux.Handle("POST /api/v1/printers/{printerID}/enable", admin(s.handleEnable(true)))
	mux.Handle("POST /api/v1/printers/{printerID}/disable", admin(s.handleEnable(false)))
	mux.Handle("GET /api/v1/bluetooth/candidates", admin(s.handleCandidates))
	mux.Handle("POST /api/v1/system/open-bluetooth-settings", admin(s.handleOpenBluetooth))
	mux.Handle("GET /api/v1/deliveries", admin(s.handleListDeliveries))
	mux.Handle("POST /api/v1/deliveries/{deliveryID}/reprint", admin(s.handleReprint))
	mux.Handle("POST /api/v1/deliveries/{deliveryID}/resolve", admin(s.handleResolve))
	mux.Handle("POST /api/v1/deliveries/{deliveryID}/cancel", admin(s.handleCancel))
	mux.Handle("GET /api/v1/logs", admin(s.handleLogs))
	mux.Handle("GET /api/v1/diagnostics", admin(s.handleDiagnostics))
	mux.Handle("GET /api/v1/diagnostics/export", admin(s.handleDiagnosticsExport))
	mux.Handle("POST /api/v1/admin/pairing-code", admin(s.handlePairingCode))
	mux.Handle("GET /api/v1/admin/tokens", admin(s.handleListTokens))
	mux.Handle("DELETE /api/v1/admin/tokens/{tokenID}", admin(s.handleRevokeToken))
	mux.Handle("GET /api/v1/admin/settings", admin(s.handleGetSettings))
	mux.Handle("PUT /api/v1/admin/settings", admin(s.handlePutSettings))

	// Embedded dashboard.
	mux.Handle("/admin/", http.StripPrefix("/admin/", webui.Handler()))
	mux.Handle("/admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	mux.Handle("/", http.RedirectHandler("/admin/", http.StatusFound))

	return s.logRequests(mux)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/v1/status" && r.URL.Path != "/api/v1/health" { // too chatty
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"))
		}
	})
}

func (s *Server) noContent(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error string `json:"error"`
}

// writeError maps service errors to HTTP statuses.
func writeError(w http.ResponseWriter, err error) {
	var ve *jobs.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: ve.Error()})
	case errors.Is(err, jobs.ErrNotFound), errors.Is(err, config.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "not found"})
	case errors.Is(err, ErrPairingRejected):
		writeJSON(w, http.StatusForbidden, errorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
	}
}
