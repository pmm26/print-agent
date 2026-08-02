// Package api exposes the local HTTP API and the embedded management UI.
// It binds to loopback only. Handlers never touch printers directly: every
// print path goes through the job service and the printer manager.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	appLogging "print-agent/internal/logging"
	"print-agent/internal/platform"
	"print-agent/internal/printers"
	"print-agent/internal/webui"
)

type Server struct {
	jobsService *jobs.Service
	jobsRepo    *jobs.Repository
	manager     *printers.Manager
	configRepo  *config.Repository
	driver      platform.Driver
	diag        *diagnostics.Service
	logs        *appLogging.Store
	auth        *AuthService
	bus         *events.Bus
	limiter     *keyedRateLimiter
	pairLimiter *rateLimiter
	log         *slog.Logger
}

func NewServer(jobsService *jobs.Service, jobsRepo *jobs.Repository, manager *printers.Manager,
	configRepo *config.Repository, driver platform.Driver, diag *diagnostics.Service,
	logs *appLogging.Store, auth *AuthService, bus *events.Bus, log *slog.Logger) *Server {
	return &Server{
		jobsService: jobsService,
		jobsRepo:    jobsRepo,
		manager:     manager,
		configRepo:  configRepo,
		driver:      driver,
		diag:        diag,
		logs:        logs,
		auth:        auth,
		bus:         bus,
		limiter:     newKeyedRateLimiter(20, 40),
		pairLimiter: newRateLimiter(0.2, 5),
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
	mux.Handle("GET /api/v1/jobs", pos(s.handleListJobs))
	mux.Handle("GET /api/v1/jobs/{jobUID}", pos(s.handleGetJob))
	mux.Handle("OPTIONS /api/v1/jobs/{jobUID}", pos(s.noContent))
	mux.Handle("GET /api/v1/print-runs/{runUID}", pos(s.handleGetPrintRun))
	mux.Handle("OPTIONS /api/v1/print-runs/{runUID}", pos(s.noContent))
	mux.Handle("POST /api/v1/jobs/{jobUID}/reprint", pos(s.handleReprint))
	mux.Handle("OPTIONS /api/v1/jobs/{jobUID}/reprint", pos(s.noContent))
	mux.Handle("GET /api/v1/status", pos(s.handleStatus))
	mux.Handle("OPTIONS /api/v1/status", pos(s.noContent))
	mux.Handle("POST /api/v1/pair", chain(http.HandlerFunc(s.handlePair), s.posCORS, s.pairRateLimit, s.limitBody))
	mux.Handle("OPTIONS /api/v1/pair", chain(http.HandlerFunc(s.noContent), s.posCORS))

	// Health is unauthenticated but still uses the configured browser origin.
	mux.Handle("GET /api/v1/health", chain(http.HandlerFunc(s.handleHealth), s.posCORS, s.rateLimit))
	mux.Handle("OPTIONS /api/v1/health", chain(http.HandlerFunc(s.noContent), s.posCORS))

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
	mux.Handle("GET /api/v1/bluetooth/devices", admin(s.handleBluetoothDevices))
	mux.Handle("POST /api/v1/bluetooth/discovery/start", admin(s.handleStartBluetoothDiscovery))
	mux.Handle("POST /api/v1/bluetooth/discovery/stop", admin(s.handleStopBluetoothDiscovery))
	mux.Handle("POST /api/v1/bluetooth/devices/{address}/pair", admin(s.handlePairBluetoothDevice))
	mux.Handle("POST /api/v1/bluetooth/devices/{address}/disconnect", admin(s.handleDisconnectBluetoothDevice))
	mux.Handle("DELETE /api/v1/bluetooth/devices/{address}", admin(s.handleForgetBluetoothDevice))
	mux.Handle("POST /api/v1/system/open-bluetooth-settings", admin(s.handleOpenBluetooth))
	mux.Handle("GET /api/v1/printers/{printerID}/queue", admin(s.handlePrinterQueue))
	mux.Handle("POST /api/v1/print-runs/{runUID}/confirm-printed", admin(s.handleConfirmPrinted))
	mux.Handle("POST /api/v1/print-runs/{runUID}/cancel", admin(s.handleCancelRun))
	mux.Handle("POST /api/v1/jobs/{jobUID}/cancel", admin(s.handleCancelJob))
	mux.Handle("GET /api/v1/logs", admin(s.handleLogs))
	mux.Handle("GET /api/v1/system-logs", admin(s.handleSystemLogs))
	mux.Handle("GET /api/v1/printer-logs", admin(s.handlePrinterLogs))
	mux.Handle("GET /api/v1/diagnostics", admin(s.handleDiagnostics))
	mux.Handle("GET /api/v1/diagnostics/export", admin(s.handleDiagnosticsExport))
	mux.Handle("POST /api/v1/admin/pairing-code", admin(s.handlePairingCode))
	mux.Handle("GET /api/v1/admin/tokens", admin(s.handleListTokens))
	mux.Handle("DELETE /api/v1/admin/tokens/{tokenID}", admin(s.handleRevokeToken))
	mux.Handle("GET /api/v1/admin/settings", admin(s.handleGetSettings))
	mux.Handle("PUT /api/v1/admin/settings", admin(s.handlePutSettings))

	// Embedded dashboard.
	mux.Handle("/admin/", http.StripPrefix("/admin", webui.Handler()))
	mux.Handle("/admin", http.RedirectHandler("/admin/operations/jobs", http.StatusFound))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, errorResponse{Code: "not_found", Error: "not found"})
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	return s.logRequests(mux)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&loggingResponseWriter{ResponseWriter: w, log: s.log}, r)
		if r.URL.Path != "/api/v1/status" && r.URL.Path != "/api/v1/health" { // too chatty
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"))
		}
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	log *slog.Logger
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *loggingResponseWriter) logError(err error) {
	w.log.Error("API request failed", "error", err)
}

func (s *Server) noContent(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, dst any) error {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		return errors.New("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

type errorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// writeError maps service errors to HTTP statuses.
func writeError(w http.ResponseWriter, err error) {
	var ve *jobs.ValidationError
	var ce *jobs.ConflictError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_request", Error: ve.Error()})
	case errors.As(err, &ce):
		code := ce.Code
		if code == "" {
			code = "conflict"
		}
		writeJSON(w, http.StatusConflict, errorResponse{Code: code, Error: ce.Error()})
	case errors.Is(err, jobs.ErrNotFound), errors.Is(err, config.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Code: "not_found", Error: "not found"})
	case errors.Is(err, ErrPairingRejected):
		writeJSON(w, http.StatusForbidden, errorResponse{Code: "pairing_rejected", Error: err.Error()})
	default:
		if logger, ok := w.(interface{ logError(error) }); ok {
			logger.logError(err)
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Code: "internal_error", Error: "internal server error"})
	}
}

func (s *Server) writeBluetoothError(w http.ResponseWriter, address string, err error) {
	s.log.Error("Bluetooth pairing failed", "address", address, "error", err)
	switch {
	case errors.Is(err, platform.ErrInvalidBluetoothAddress):
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_address", Error: "invalid Bluetooth device address"})
	case errors.Is(err, platform.ErrBluetoothDeviceNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Code: "bluetooth_device_not_found", Error: "Bluetooth device is no longer available; scan again"})
	case errors.Is(err, platform.ErrBluetoothPairInProgress):
		writeJSON(w, http.StatusConflict, errorResponse{Code: "pairing_in_progress", Error: "Bluetooth pairing is already in progress"})
	case errors.Is(err, platform.ErrBluetoothPairRejected):
		writeJSON(w, http.StatusConflict, errorResponse{Code: "pairing_failed", Error: "pairing was rejected; put the printer in pairing mode and verify its PIN"})
	case errors.Is(err, platform.ErrBluetoothPairTimeout):
		writeJSON(w, http.StatusGatewayTimeout, errorResponse{Code: "pairing_timeout", Error: "pairing timed out; put the printer in pairing mode and try again"})
	case errors.Is(err, platform.ErrBluetoothUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Code: "bluetooth_unavailable", Error: "Bluetooth is unavailable; check that the adapter is powered on"})
	case errors.Is(err, platform.ErrBluetoothNotAuthorized):
		writeJSON(w, http.StatusForbidden, errorResponse{Code: "bluetooth_not_authorized", Error: "this session is not authorized to manage Bluetooth; use bluetoothctl or the system Bluetooth settings"})
	case errors.Is(err, platform.ErrBluetoothProtocolUnsupported):
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Code: "connection_type_unsupported", Error: err.Error()})
	default:
		writeJSON(w, http.StatusBadGateway, errorResponse{Code: "bluetooth_error", Error: "Bluetooth pairing failed; check the agent log for details"})
	}
}

func (s *Server) writeBluetoothManagementError(w http.ResponseWriter, address, action string, err error) {
	s.log.Error("Bluetooth management failed", "action", action, "address", address, "error", err)
	switch {
	case errors.Is(err, platform.ErrInvalidBluetoothAddress):
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_address", Error: "invalid Bluetooth device address"})
	case errors.Is(err, platform.ErrBluetoothDeviceNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Code: "bluetooth_device_not_found", Error: "Bluetooth device is no longer available; scan again"})
	case errors.Is(err, platform.ErrBluetoothNotAuthorized):
		writeJSON(w, http.StatusForbidden, errorResponse{Code: "bluetooth_not_authorized", Error: "this session is not authorized to manage Bluetooth; use bluetoothctl or the system Bluetooth settings"})
	default:
		writeJSON(w, http.StatusBadGateway, errorResponse{Code: "bluetooth_error", Error: fmt.Sprintf("BlueZ could not %s the device; check the agent log for details", action)})
	}
}
