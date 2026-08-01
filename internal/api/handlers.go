package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/jobs"
	"print-agent/internal/printers"
)

// ---- jobs (POS-facing) ----

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req jobs.CreatePrintJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" && key != req.JobID {
		writeJSON(w, http.StatusBadRequest,
			errorResponse{Error: "Idempotency-Key header must match jobId"})
		return
	}
	req.Source = r.Header.Get("Origin")
	if req.Source == "" {
		req.Source = "local"
	}
	result, err := s.jobsService.Accept(req)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusAccepted
	if result.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	result, err := s.jobsService.Get(r.PathValue("jobID"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.manager.Statuses()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent":    s.diag.Report(),
		"printers": statuses,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- pairing (POS-facing) ----

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON"})
		return
	}
	origin := r.Header.Get("Origin")
	token, err := s.auth.Pair(body.Code, origin, body.Label)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// ---- printers (management) ----

func (s *Server) handleListPrinters(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.manager.Statuses()
	if err != nil {
		writeError(w, err)
		return
	}
	if statuses == nil {
		statuses = []printers.Status{}
	}
	writeJSON(w, http.StatusOK, statuses)
}

// printerRequest wraps PrinterConfig so omitted booleans default to true
// (Go's zero value would silently disable new printers and their reconnect).
type printerRequest struct {
	config.PrinterConfig
	Enabled       *bool `json:"enabled"`
	AutoReconnect *bool `json:"autoReconnect"`
}

func (s *Server) decodePrinter(r *http.Request, existingID string) (config.PrinterConfig, error) {
	var req printerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return config.PrinterConfig{}, jobs.NewValidationError("invalid JSON: " + err.Error())
	}
	cfg := req.PrinterConfig
	cfg.Enabled = req.Enabled == nil || *req.Enabled
	cfg.AutoReconnect = req.AutoReconnect == nil || *req.AutoReconnect
	if existingID != "" {
		cfg.ID = existingID
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return cfg, jobs.NewValidationError(err.Error())
	}
	if !escposEncodingOK(cfg.Encoding) {
		return cfg, jobs.NewValidationError(fmt.Sprintf("unsupported encoding %q", cfg.Encoding))
	}
	return cfg, nil
}

func (s *Server) handleCreatePrinter(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.decodePrinter(r, "")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.configRepo.GetPrinter(cfg.ID); err == nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "printer already exists; use PUT to update"})
		return
	}
	if err := s.manager.ApplyPrinter(cfg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, cfg)
}

func (s *Server) handleUpdatePrinter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("printerID")
	if _, err := s.configRepo.GetPrinter(id); err != nil {
		writeError(w, err)
		return
	}
	cfg, err := s.decodePrinter(r, id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.manager.ApplyPrinter(cfg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleDeletePrinter(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.RemovePrinter(r.PathValue("printerID")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.Reconnect(r.PathValue("printerID")); err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reconnecting": true})
}

func (s *Server) handleReconnectAll(w http.ResponseWriter, r *http.Request) {
	s.manager.ReconnectAll()
	writeJSON(w, http.StatusOK, map[string]bool{"reconnecting": true})
}

func (s *Server) handleEnable(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.manager.SetEnabled(r.PathValue("printerID"), enabled); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
	}
}

func (s *Server) handleTestPrint(w http.ResponseWriter, r *http.Request) {
	printerID := r.PathValue("printerID")
	if _, err := s.configRepo.GetPrinter(printerID); err != nil {
		writeError(w, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102-150405.000")
	payload, _ := json.Marshal(map[string]string{
		"printerId": printerID,
		"line":      "manual test print",
	})
	result, err := s.jobsService.Accept(jobs.CreatePrintJobRequest{
		JobID:  fmt.Sprintf("local:test:%s:%s", printerID, stamp),
		Source: "dashboard",
		Documents: []jobs.DocumentRequest{{
			DeliveryID: fmt.Sprintf("local:test:%s:%s:test-page", printerID, stamp),
			PrinterID:  printerID,
			Template:   escpos.TemplateTestPage,
			Data:       payload,
		}},
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

// ---- discovery (management) ----

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	cands, err := s.connector.ListCandidates(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cands)
}

func (s *Server) handleOpenBluetooth(w http.ResponseWriter, r *http.Request) {
	if err := s.connector.OpenSystemBluetoothSettings(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"opened": true})
}

// ---- deliveries / queue (management) ----

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	deliveries, err := s.jobsRepo.ListDeliveries(q.Get("printerId"), jobs.DeliveryStatus(q.Get("status")), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if deliveries == nil {
		deliveries = []jobs.Delivery{}
	}
	writeJSON(w, http.StatusOK, deliveries)
}

func (s *Server) handleReprint(w http.ResponseWriter, r *http.Request) {
	d, err := s.jobsService.Reprint(r.PathValue("deliveryID"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	if err := s.jobsService.Resolve(r.PathValue("deliveryID")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"resolved": true})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.jobsService.Cancel(r.PathValue("deliveryID")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

// ---- diagnostics (management) ----

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.diag.RecentEvents(limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	statuses, _ := s.manager.Statuses()
	cands, _ := s.connector.ListCandidates(r.Context())
	allowed, _ := s.configRepo.GetSetting(config.SettingAllowedOrigin)
	writeJSON(w, http.StatusOK, map[string]any{
		"report":        s.diag.Report(),
		"printers":      statuses,
		"candidates":    cands,
		"allowedOrigin": allowed,
		"hasClientToken": s.auth.HasActiveToken(),
		"templates":     escpos.TemplateNames(),
		"encodings":     escpos.SupportedEncodings(),
	})
}

func (s *Server) handleDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	statuses, _ := s.manager.Statuses()
	allowed, _ := s.configRepo.GetSetting(config.SettingAllowedOrigin)
	tokens, _ := s.auth.ListTokens() // metadata only, never secrets
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="print-agent-diagnostics-%s.zip"`, time.Now().Format("20060102-1504")))
	s.diag.WriteExportZip(w, map[string]any{
		"printers.json": statuses,
		"settings.json": map[string]any{"allowedOrigin": allowed},
		"tokens.json":   tokens,
	})
}

// ---- pairing & settings (management) ----

func (s *Server) handlePairingCode(w http.ResponseWriter, r *http.Request) {
	code, expires := s.auth.GeneratePairingCode()
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expiresAt": expires})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.auth.ListTokens()
	if err != nil {
		writeError(w, err)
		return
	}
	if tokens == nil {
		tokens = []TokenInfo{}
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.RevokeToken(r.PathValue("tokenID")); err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	allowed, err := s.configRepo.GetSetting(config.SettingAllowedOrigin)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"allowedOrigin": allowed})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AllowedOrigin string `json:"allowedOrigin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON"})
		return
	}
	if err := s.configRepo.SetSetting(config.SettingAllowedOrigin, body.AllowedOrigin); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"allowedOrigin": body.AllowedOrigin})
}

func escposEncodingOK(name string) bool {
	_, err := escpos.NewBuilder(name, 32)
	return err == nil
}
