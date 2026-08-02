package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	appLogging "print-agent/internal/logging"
	"print-agent/internal/platform"
	"print-agent/internal/printers"
)

// ---- Jobs and Print Runs (POS-facing) ----

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req jobs.CreateJobRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	req.Owner = requestOwner(r)
	req.Source = req.Owner
	result, err := s.jobsService.Accept(req)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusAccepted
	if result.Duplicate {
		status = http.StatusOK
	}
	w.Header().Set("Location", "/api/v2/jobs/"+result.UID)
	writeJSON(w, status, result)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	result, err := s.jobsService.Get(r.PathValue("jobUID"), requestOwner(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func decodeJobCursor(raw string) (*time.Time, string, error) {
	if raw == "" {
		return nil, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, "", jobs.NewValidationError("invalid cursor")
	}
	parts := strings.SplitN(string(b), "\x00", 2)
	if len(parts) != 2 {
		return nil, "", jobs.NewValidationError("invalid cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, "", jobs.NewValidationError("invalid cursor")
	}
	return &t, parts[1], nil
}

func encodeJobCursor(created time.Time, uid string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(created.UTC().Format(time.RFC3339Nano) + "\x00" + uid))
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, beforeUID, err := decodeJobCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, err)
		return
	}
	attention := r.URL.Query().Get("attention")
	if attention != "" && attention != "true" && attention != "false" {
		writeError(w, jobs.NewValidationError("attention must be true or false"))
		return
	}
	result, err := s.jobsService.List(r.URL.Query().Get("jobId"), r.URL.Query().Get("template"),
		attention, requestOwner(r), limit, before, beforeUID)
	if err != nil {
		writeError(w, err)
		return
	}
	if result == nil {
		result = []jobs.JobListItem{}
	}
	next := ""
	effectiveLimit := limit
	if effectiveLimit <= 0 {
		effectiveLimit = 50
	} else if effectiveLimit > 100 {
		effectiveLimit = 100
	}
	if len(result) == effectiveLimit {
		last := result[len(result)-1]
		next = encodeJobCursor(last.CreatedAt, last.UID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": result, "nextCursor": next})
}

func (s *Server) handleGetPrintRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.jobsService.GetRun(r.PathValue("runUID"), requestOwner(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.manager.Statuses()
	if err != nil {
		s.log.Error("status snapshot failed", "error", err)
		statuses = []printers.Status{}
	}
	response := map[string]any{
		"agent":       s.diag.Report(),
		"platform":    s.driver.Name(),
		"printers":    statuses,
		"persistence": s.manager.PersistenceStatus(),
		"degraded":    err != nil,
	}
	if s.runtimeStatus != nil {
		response["events"] = s.runtimeStatus(r.Context())
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.handleReadiness(w, r)
}

func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	report := s.diag.Report()
	persistence := s.manager.PersistenceStatus()
	diskPressure := false
	if s.runtimeStatus != nil {
		if value, ok := s.runtimeStatus(r.Context())["diskPressure"].(bool); ok {
			diskPressure = value
		}
	}
	status := http.StatusOK
	if !report.DatabaseOK || persistence.Paused || diskPressure {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": report.DatabaseOK && !persistence.Paused && !diskPressure,
		"databaseOk": report.DatabaseOK, "persistencePaused": persistence.Paused, "eventDiskPressure": diskPressure})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.manager.Statuses()
	if err != nil {
		writeError(w, err)
		return
	}
	var queued, attention int
	for _, status := range statuses {
		queued += status.QueueDepth
		attention += status.AttentionCount
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE print_agent_queue_depth gauge\nprint_agent_queue_depth %d\n", queued)
	fmt.Fprintf(w, "# TYPE print_agent_attention_runs gauge\nprint_agent_attention_runs %d\n", attention)
	if s.runtimeStatus != nil {
		runtime := s.runtimeStatus(r.Context())
		for key, metric := range map[string]string{"eventCount": "print_agent_event_log_depth", "pendingDeliveries": "print_agent_outbox_depth", "deadLetterCount": "print_agent_dead_letter_count", "droppedOperationalEvents": "print_agent_event_dropped_total"} {
			if value, ok := runtime[key]; ok {
				fmt.Fprintf(w, "# TYPE %s gauge\n%s %v\n", metric, metric, value)
			}
		}
	}
}

// ---- pairing (POS-facing) ----

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	origin := r.Header.Get("Origin")
	normalizedOrigin, err := normalizeOrigin(origin)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_origin", Error: err.Error()})
		return
	}
	token, err := s.auth.Pair(body.Code, normalizedOrigin, body.Label)
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

// printerRequest uses pointers so updates preserve every omitted field.
type printerRequest struct {
	ID                   *string                      `json:"id"`
	DisplayName          *string                      `json:"displayName"`
	Enabled              *bool                        `json:"enabled"`
	Transport            *config.TransportKind        `json:"transport"`
	DeviceAddress        *string                      `json:"deviceAddress"`
	Endpoint             *string                      `json:"endpoint"`
	BaudRate             *int                         `json:"baudRate"`
	DataBits             *int                         `json:"dataBits"`
	StopBits             *int                         `json:"stopBits"`
	Parity               *string                      `json:"parity"`
	CharactersPerLine    *int                         `json:"charactersPerLine"`
	Encoding             *string                      `json:"encoding"`
	AutoReconnect        *bool                        `json:"autoReconnect"`
	ConnectionPreference *config.ConnectionPreference `json:"connectionPreference"`
}

func decodePrinterRequest(r *http.Request) (printerRequest, error) {
	var req printerRequest
	if err := decodeJSON(r, &req); err != nil {
		return req, jobs.NewValidationError("invalid JSON: " + err.Error())
	}
	return req, nil
}

func (s *Server) mergePrinter(req printerRequest, cfg config.PrinterConfig, existingID string) (config.PrinterConfig, error) {
	if existingID != "" && req.ID != nil && *req.ID != existingID {
		return cfg, jobs.NewValidationError("id in request body must match printerID in URL")
	}
	if req.ID != nil && existingID == "" {
		cfg.ID = *req.ID
	}
	if req.DisplayName != nil {
		cfg.DisplayName = *req.DisplayName
	}
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.Transport != nil {
		cfg.Transport = *req.Transport
	}
	if req.DeviceAddress != nil {
		cfg.DeviceAddress = *req.DeviceAddress
	}
	if req.Endpoint != nil {
		cfg.Endpoint = *req.Endpoint
	}
	if req.BaudRate != nil {
		cfg.BaudRate = *req.BaudRate
	}
	if req.DataBits != nil {
		cfg.DataBits = *req.DataBits
	}
	if req.StopBits != nil {
		cfg.StopBits = *req.StopBits
	}
	if req.Parity != nil {
		cfg.Parity = *req.Parity
	}
	if req.CharactersPerLine != nil {
		cfg.CharactersPerLine = *req.CharactersPerLine
	}
	if req.Encoding != nil {
		cfg.Encoding = *req.Encoding
	}
	if req.AutoReconnect != nil {
		cfg.AutoReconnect = *req.AutoReconnect
	}
	if req.ConnectionPreference != nil {
		cfg.ConnectionPreference = *req.ConnectionPreference
	}
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

func (s *Server) decodePrinter(r *http.Request) (config.PrinterConfig, error) {
	req, err := decodePrinterRequest(r)
	if err != nil {
		return config.PrinterConfig{}, err
	}
	cfg := config.PrinterConfig{Enabled: true, AutoReconnect: true}
	return s.mergePrinter(req, cfg, "")
}

func (s *Server) handleCreatePrinter(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.decodePrinter(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.manager.CreatePrinter(cfg); err != nil {
		writeError(w, err)
		return
	}
	saved, err := s.configRepo.GetPrinter(cfg.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) handleUpdatePrinter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("printerID")
	req, err := decodePrinterRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	cfg, err := s.manager.UpdatePrinterWith(id, func(existing config.PrinterConfig) (config.PrinterConfig, error) {
		return s.mergePrinter(req, existing, id)
	})
	if err != nil {
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
		writeJSON(w, http.StatusConflict, errorResponse{Code: "printer_not_active", Error: err.Error()})
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
	result, err := s.jobsService.Accept(jobs.CreateJobRequest{
		JobID: fmt.Sprintf("local:test:%s:%s", printerID, stamp), Source: "dashboard", Owner: "local",
		Template: escpos.TemplateTestPage, Data: payload, PrinterIDs: []string{printerID},
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

// ---- discovery (management) ----

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	cands, err := s.driver.ListCandidates(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cands)
}

func (s *Server) bluetoothPairer() (platform.BluetoothPairer, bool) {
	pairer, ok := s.driver.(platform.BluetoothPairer)
	return pairer, ok
}

func (s *Server) handleBluetoothDevices(w http.ResponseWriter, r *http.Request) {
	pairer, supported := s.bluetoothPairer()
	if !supported {
		writeJSON(w, http.StatusOK, map[string]any{
			"supported": false,
			"devices":   []platform.BluetoothDevice{},
		})
		return
	}
	devices, err := pairer.ListBluetoothDevices(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if devices == nil {
		devices = []platform.BluetoothDevice{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"supported": true, "devices": devices})
}

func (s *Server) handleStartBluetoothDiscovery(w http.ResponseWriter, r *http.Request) {
	pairer, supported := s.bluetoothPairer()
	if !supported {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Code: "not_supported", Error: "in-app Bluetooth discovery is not supported on this platform"})
		return
	}
	preference := config.ConnectionPreference(r.URL.Query().Get("connectionType"))
	if preference == "" {
		preference = config.ConnectionAuto
	}
	if preference != config.ConnectionAuto && preference != config.ConnectionRFCOMM && preference != config.ConnectionBLE {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_connection_type", Error: "connectionType must be auto, rfcomm, or ble"})
		return
	}
	if err := pairer.StartBluetoothDiscovery(r.Context(), preference); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"discovering": true})
}

func (s *Server) handleStopBluetoothDiscovery(w http.ResponseWriter, r *http.Request) {
	pairer, supported := s.bluetoothPairer()
	if !supported {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Code: "not_supported", Error: "in-app Bluetooth discovery is not supported on this platform"})
		return
	}
	if err := pairer.StopBluetoothDiscovery(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"discovering": false})
}

func (s *Server) handlePairBluetoothDevice(w http.ResponseWriter, r *http.Request) {
	pairer, supported := s.bluetoothPairer()
	if !supported {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Code: "not_supported", Error: "in-app Bluetooth pairing is not supported on this platform"})
		return
	}
	var body struct {
		PIN            string                      `json:"pin"`
		ConnectionType config.ConnectionPreference `json:"connectionType"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	if body.ConnectionType == "" {
		body.ConnectionType = config.ConnectionAuto
	}
	if body.ConnectionType != config.ConnectionAuto && body.ConnectionType != config.ConnectionRFCOMM && body.ConnectionType != config.ConnectionBLE {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_connection_type", Error: "connectionType must be auto, rfcomm, or ble"})
		return
	}
	address := r.PathValue("address")
	device, err := pairer.PairBluetoothDevice(r.Context(), address, body.PIN, body.ConnectionType)
	if err != nil {
		s.writeBluetoothError(w, address, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":  device.Endpoint != "",
		"device": device,
	})
}

func (s *Server) configuredPrinterForAddress(address string) (string, error) {
	statuses, err := s.manager.Statuses()
	if err != nil {
		return "", err
	}
	wanted := strings.ToUpper(strings.ReplaceAll(address, "-", ":"))
	for _, status := range statuses {
		candidate := status.Printer.DeviceAddress
		if candidate == "" {
			parts := strings.SplitN(status.Printer.Endpoint, "://", 2)
			if len(parts) == 2 {
				candidate = parts[1]
			}
		}
		if strings.ToUpper(strings.ReplaceAll(candidate, "-", ":")) == wanted {
			return status.Printer.ID, nil
		}
	}
	return "", nil
}

func (s *Server) handleDisconnectBluetoothDevice(w http.ResponseWriter, r *http.Request) {
	pairer, supported := s.bluetoothPairer()
	if !supported {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Code: "not_supported", Error: "Bluetooth management is not supported"})
		return
	}
	address := r.PathValue("address")
	if err := pairer.DisconnectBluetoothDevice(r.Context(), address); err != nil {
		s.writeBluetoothManagementError(w, address, "disconnect", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"disconnected": true})
}

func (s *Server) handleForgetBluetoothDevice(w http.ResponseWriter, r *http.Request) {
	pairer, supported := s.bluetoothPairer()
	if !supported {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Code: "not_supported", Error: "Bluetooth management is not supported"})
		return
	}
	address := r.PathValue("address")
	printerID, err := s.configuredPrinterForAddress(address)
	if err != nil {
		writeError(w, err)
		return
	}
	if printerID != "" {
		writeJSON(w, http.StatusConflict, errorResponse{Code: "device_in_use", Error: fmt.Sprintf("remove printer %s before forgetting this Bluetooth device", printerID)})
		return
	}
	if err := pairer.ForgetBluetoothDevice(r.Context(), address); err != nil {
		s.writeBluetoothManagementError(w, address, "forget", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"forgotten": true})
}

func (s *Server) handleOpenBluetooth(w http.ResponseWriter, r *http.Request) {
	if err := s.driver.OpenSystemBluetoothSettings(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"opened": true})
}

// ---- Print Run queues and operator actions ----

func (s *Server) handlePrinterQueue(w http.ResponseWriter, r *http.Request) {
	printerID := r.PathValue("printerID")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.jobsRepo.ListPrinterRuns(printerID, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	var active *jobs.PrintRun
	queued := []jobs.PrintRun{}
	retryPending := []jobs.PrintRun{}
	attention := []jobs.PrintRun{}
	recentTransmitted := []jobs.PrintRun{}
	for i := range runs {
		run := runs[i]
		switch {
		case run.Status == jobs.RunClaimed || run.Status == jobs.RunTransmitting:
			copy := run
			active = &copy
		case run.Status == jobs.RunQueued:
			queued = append(queued, run)
		case run.Status == jobs.RunFailed && run.Retryable && run.Resolution == "":
			retryPending = append(retryPending, run)
		case (run.Status == jobs.RunFailed || run.Status == jobs.RunUncertain) && run.Resolution == "":
			attention = append(attention, run)
		case run.Status == jobs.RunTransmitted && len(recentTransmitted) < 20:
			recentTransmitted = append(recentTransmitted, run)
		}
	}
	statuses, err := s.manager.Statuses()
	if err != nil {
		writeError(w, err)
		return
	}
	var printerStatus any
	for _, status := range statuses {
		if status.Printer.ID == printerID {
			printerStatus = status
			break
		}
	}
	if printerStatus == nil {
		writeError(w, config.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"printer": printerStatus, "activeRun": active, "queuedRuns": queued,
		"retryPendingRuns": retryPending, "attentionRuns": attention,
		"recentTransmittedRuns": recentTransmitted,
	})
}

func (s *Server) handleReprint(w http.ResponseWriter, r *http.Request) {
	var req jobs.ReprintRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	result, err := s.jobsService.Reprint(r.PathValue("jobUID"), req, requestOwner(r))
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

func (s *Server) handleConfirmPrinted(w http.ResponseWriter, r *http.Request) {
	if err := s.jobsService.ConfirmPrinted(r.PathValue("runUID")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"confirmedPrinted": true})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if err := s.jobsService.CancelRun(r.PathValue("runUID")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PrinterIDs []string `json:"printerIds"`
		Reason     string   `json:"reason,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	if err := s.jobsService.CancelJobTargets(r.PathValue("jobUID"), body.PrinterIDs, body.Reason); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

// ---- diagnostics (management) ----

func logPageLimit(raw string) int {
	limit, _ := strconv.Atoi(raw)
	if limit <= 0 {
		return 100
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func encodeNumericCursor(created time.Time, id int64) string {
	value := created.UTC().Format(time.RFC3339Nano) + "\x00" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeNumericCursor(raw string) (*time.Time, int64, error) {
	if raw == "" {
		return nil, 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, 0, jobs.NewValidationError("invalid cursor")
	}
	parts := strings.SplitN(string(decoded), "\x00", 2)
	if len(parts) != 2 {
		return nil, 0, jobs.NewValidationError("invalid cursor")
	}
	created, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, 0, jobs.NewValidationError("invalid cursor")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return nil, 0, jobs.NewValidationError("invalid cursor")
	}
	return &created, id, nil
}

func parseLogTime(raw, name string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, jobs.NewValidationError(name + " must be an RFC3339 timestamp")
	}
	value = value.UTC()
	return &value, nil
}

func logRange(values url.Values) (*time.Time, *time.Time, error) {
	from, err := parseLogTime(values.Get("from"), "from")
	if err != nil {
		return nil, nil, err
	}
	to, err := parseLogTime(values.Get("to"), "to")
	if err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && from.After(*to) {
		return nil, nil, jobs.NewValidationError("from must not be after to")
	}
	return from, to, nil
}

func boundedLogFilter(values url.Values, name string) (string, error) {
	value := strings.TrimSpace(values.Get(name))
	if len(value) > 250 {
		return "", jobs.NewValidationError(name + " must be at most 250 bytes")
	}
	return value, nil
}

func parseLevels(values url.Values) ([]string, error) {
	seen := map[string]bool{}
	var levels []string
	for _, raw := range values["levels"] {
		for _, level := range strings.Split(raw, ",") {
			level = strings.ToLower(strings.TrimSpace(level))
			if level == "" || seen[level] {
				continue
			}
			switch level {
			case "debug", "info", "warn", "error":
			default:
				return nil, jobs.NewValidationError("levels may contain only debug, info, warn, or error")
			}
			seen[level] = true
			levels = append(levels, level)
		}
	}
	return levels, nil
}

func (s *Server) handleSystemLogs(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	levels, err := parseLevels(values)
	if err != nil {
		writeError(w, err)
		return
	}
	from, to, err := logRange(values)
	if err != nil {
		writeError(w, err)
		return
	}
	query, err := boundedLogFilter(values, "q")
	if err != nil {
		writeError(w, err)
		return
	}
	printerID, err := boundedLogFilter(values, "printerId")
	if err != nil {
		writeError(w, err)
		return
	}
	runUID, err := boundedLogFilter(values, "runUid")
	if err != nil {
		writeError(w, err)
		return
	}
	before, beforeID, err := decodeNumericCursor(values.Get("cursor"))
	if err != nil {
		writeError(w, err)
		return
	}
	limit := logPageLimit(values.Get("limit"))
	records, err := s.logs.Query(appLogging.Filter{
		Levels: levels, Query: query, PrinterID: printerID, RunUID: runUID,
		From: from, To: to, Before: before, BeforeID: beforeID, Limit: limit + 1,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	next := ""
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		next = encodeNumericCursor(last.CreatedAt, last.ID)
	}
	if records == nil {
		records = []appLogging.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logs": records, "nextCursor": next, "retentionSeconds": int(appLogging.Retention.Seconds()),
	})
}

func (s *Server) handlePrinterLogs(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	from, to, err := logRange(values)
	if err != nil {
		writeError(w, err)
		return
	}
	query, err := boundedLogFilter(values, "q")
	if err != nil {
		writeError(w, err)
		return
	}
	printerID, err := boundedLogFilter(values, "printerId")
	if err != nil {
		writeError(w, err)
		return
	}
	runUID, err := boundedLogFilter(values, "runUid")
	if err != nil {
		writeError(w, err)
		return
	}
	eventType, err := boundedLogFilter(values, "event")
	if err != nil {
		writeError(w, err)
		return
	}
	before, beforeID, err := decodeNumericCursor(values.Get("cursor"))
	if err != nil {
		writeError(w, err)
		return
	}
	limit := logPageLimit(values.Get("limit"))
	events, err := s.diag.PrinterEvents(diagnostics.EventFilter{
		PrinterID: printerID, EventType: eventType, RunUID: runUID, Query: query,
		From: from, To: to, Before: before, BeforeID: beforeID, Limit: limit + 1,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	next := ""
	if len(events) > limit {
		events = events[:limit]
		last := events[len(events)-1]
		next = encodeNumericCursor(last.CreatedAt, last.ID)
	}
	if events == nil {
		events = []diagnostics.EventRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events, "nextCursor": next, "retentionSeconds": int(diagnostics.EventRetention.Seconds()),
	})
}

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
	var partialErrors []string
	statuses, err := s.manager.Statuses()
	if err != nil {
		partialErrors = append(partialErrors, "printers unavailable")
		s.log.Error("diagnostics printers failed", "error", err)
	}
	cands, err := s.driver.ListCandidates(r.Context())
	if err != nil {
		partialErrors = append(partialErrors, "candidates unavailable")
		s.log.Error("diagnostics candidates failed", "error", err)
	}
	allowed, err := s.configRepo.GetSetting(config.SettingAllowedOrigin)
	if err != nil {
		partialErrors = append(partialErrors, "settings unavailable")
		s.log.Error("diagnostics settings failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"report":         s.diag.Report(),
		"platform":       s.driver.Name(),
		"printers":       statuses,
		"candidates":     cands,
		"allowedOrigin":  allowed,
		"hasClientToken": s.auth.HasActiveToken(),
		"templates":      escpos.TemplateNames(),
		"encodings":      escpos.SupportedEncodings(),
		"partialErrors":  partialErrors,
	})
}

func (s *Server) handleDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.manager.Statuses()
	if err != nil {
		writeError(w, err)
		return
	}
	allowed, err := s.configRepo.GetSetting(config.SettingAllowedOrigin)
	if err != nil {
		writeError(w, err)
		return
	}
	tokens, err := s.auth.ListTokens() // metadata only, never secrets
	if err != nil {
		writeError(w, err)
		return
	}
	var archive bytes.Buffer
	if err := s.diag.WriteExportZip(&archive, map[string]any{
		"printers.json": statuses,
		"settings.json": map[string]any{"allowedOrigin": allowed},
		"tokens.json":   tokens,
	}); err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="print-agent-diagnostics-%s.zip"`, time.Now().Format("20060102-1504")))
	w.Header().Set("Content-Length", strconv.Itoa(archive.Len()))
	_, _ = w.Write(archive.Bytes())
}

// ---- pairing & settings (management) ----

func (s *Server) handlePairingCode(w http.ResponseWriter, r *http.Request) {
	code, expires, err := s.auth.GeneratePairingCode()
	if err != nil {
		writeError(w, err)
		return
	}
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
		writeJSON(w, http.StatusNotFound, errorResponse{Code: "not_found", Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) handleEventTicket(w http.ResponseWriter, _ *http.Request) {
	ticket, expires, err := s.auth.IssueEventTicket()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": ticket, "expiresAt": expires})
}

func (s *Server) handleCreateEventCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	if len(body.Label) > 100 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_request", Error: "label is too long"})
		return
	}
	token, err := s.auth.CreateEventCredential(body.Label)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"token": token})
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
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	origin, err := normalizeOrigin(body.AllowedOrigin)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_origin", Error: err.Error()})
		return
	}
	if err := s.configRepo.SetSetting(config.SettingAllowedOrigin, origin); err != nil {
		writeError(w, err)
		return
	}
	s.bus.Publish(events.Event{Type: events.ConfigChanged,
		Metadata: map[string]any{"setting": "allowedPosOrigin", "restartRequired": false}})
	writeJSON(w, http.StatusOK, map[string]string{"allowedOrigin": origin})
}

func requestOwner(r *http.Request) string {
	origin := r.Header.Get("Origin")
	if origin == "" || isLocalOrigin(origin) {
		return "local"
	}
	return origin
}

func normalizeOrigin(raw string) (string, error) {
	if raw == "" || raw == "null" || strings.TrimSpace(raw) != raw {
		return "", jobs.NewValidationError("origin must be a non-empty HTTP(S) origin")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", jobs.NewValidationError("origin must contain only an http(s) scheme, host, and optional port")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

func escposEncodingOK(name string) bool {
	_, err := escpos.NewBuilder(name, 32)
	return err == nil
}
