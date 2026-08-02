package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	appLogging "print-agent/internal/logging"
	"print-agent/internal/platform"
	"print-agent/internal/printers"
	"print-agent/internal/storage"
)

const posOrigin = "https://pos.example.com"

type stubDriver struct{ platform.UnimplementedDriver }

type pairingStubDriver struct {
	stubDriver
	result       platform.BluetoothDevice
	pairErr      error
	address      string
	pin          string
	disconnected string
	forgotten    string
}

func (d *pairingStubDriver) ListBluetoothDevices(context.Context) ([]platform.BluetoothDevice, error) {
	return []platform.BluetoothDevice{d.result}, nil
}

func (d *pairingStubDriver) StartBluetoothDiscovery(context.Context, config.ConnectionPreference) error {
	return nil
}
func (d *pairingStubDriver) StopBluetoothDiscovery(context.Context) error { return nil }
func (d *pairingStubDriver) PairBluetoothDevice(_ context.Context, address, pin string, _ config.ConnectionPreference) (platform.BluetoothDevice, error) {
	d.address = address
	d.pin = pin
	return d.result, d.pairErr
}
func (d *pairingStubDriver) DisconnectBluetoothDevice(_ context.Context, address string) error {
	d.disconnected = address
	return nil
}
func (d *pairingStubDriver) ForgetBluetoothDevice(_ context.Context, address string) error {
	d.forgotten = address
	return nil
}

func newTestServer(t *testing.T) (*httptest.Server, *AuthService, *jobs.Repository, *appLogging.Store) {
	return newTestServerWithDriver(t, stubDriver{}, slog.New(slog.DiscardHandler))
}

func newTestServerWithDriver(t *testing.T, driver platform.Driver, logger *slog.Logger) (*httptest.Server, *AuthService, *jobs.Repository, *appLogging.Store) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bus := events.NewBus()
	configRepo := config.NewRepository(db)
	configRepo.SetSetting(config.SettingAllowedOrigin, posOrigin)
	cfg := config.PrinterConfig{ID: "cashier", Enabled: false, Transport: config.TransportMock, Endpoint: "mock://c"}
	cfg.ApplyDefaults()
	if err := configRepo.SavePrinter(cfg); err != nil {
		t.Fatal(err)
	}
	repo := jobs.NewRepository(db)
	manager := printers.NewManager(configRepo, repo, driver, bus, nil)
	svc := jobs.NewService(repo, bus, manager, escpos.NewRenderer(), escpos.KnownTemplate)
	svc.SetWaker(manager)
	svc.SetPayloadValidator(escpos.ValidateTemplateData)
	auth := NewAuthService(db)
	diag := diagnostics.NewService(db, "test.db")
	logStore := appLogging.NewStore(db)
	server := NewServer(svc, repo, manager, configRepo, driver, diag, logStore, auth, bus, logger)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts, auth, repo, logStore
}

func TestPairBluetoothDeviceReturnsTruthfulReadyDevice(t *testing.T) {
	driver := &pairingStubDriver{result: platform.BluetoothDevice{
		Name: "BlueTooth Printer", Address: "5A:4A:95:56:6F:B6", Connected: true,
		IsPrinter: true, Endpoint: "ble://5A:4A:95:56:6F:B6",
	}}
	ts, _, _, _ := newTestServerWithDriver(t, driver, slog.New(slog.DiscardHandler))
	resp := request(t, http.MethodPost, ts.URL+"/api/v1/bluetooth/devices/5A%3A4A%3A95%3A56%3A6F%3AB6/pair", `{"pin":"0000"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var result struct {
		Ready  bool                     `json:"ready"`
		Device platform.BluetoothDevice `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.Device.Paired || result.Device.Endpoint != driver.result.Endpoint {
		t.Fatalf("result = %#v", result)
	}
	if driver.address != "5A:4A:95:56:6F:B6" || driver.pin != "0000" {
		t.Fatalf("pair call = address %q, pin %q", driver.address, driver.pin)
	}
}

func TestBluetoothDisconnectAndForgetManagement(t *testing.T) {
	driver := &pairingStubDriver{}
	ts, _, _, _ := newTestServerWithDriver(t, driver, slog.New(slog.DiscardHandler))
	address := "5A%3A4A%3A95%3A56%3A6F%3AB6"
	resp := request(t, http.MethodPost, ts.URL+"/api/v1/bluetooth/devices/"+address+"/disconnect", ``, nil)
	if resp.StatusCode != http.StatusOK || driver.disconnected != "5A:4A:95:56:6F:B6" {
		t.Fatalf("disconnect = %d %q", resp.StatusCode, driver.disconnected)
	}
	resp = request(t, http.MethodDelete, ts.URL+"/api/v1/bluetooth/devices/"+address, ``, nil)
	if resp.StatusCode != http.StatusOK || driver.forgotten != "5A:4A:95:56:6F:B6" {
		t.Fatalf("forget = %d %q", resp.StatusCode, driver.forgotten)
	}
}

func TestForgetConfiguredBluetoothDeviceIsBlocked(t *testing.T) {
	driver := &pairingStubDriver{}
	ts, _, _, _ := newTestServerWithDriver(t, driver, slog.New(slog.DiscardHandler))
	resp := request(t, http.MethodPut, ts.URL+"/api/v1/printers/cashier",
		`{"deviceAddress":"5A:4A:95:56:6F:B6","endpoint":"ble://5A:4A:95:56:6F:B6"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}
	resp = request(t, http.MethodDelete, ts.URL+"/api/v1/bluetooth/devices/5A%3A4A%3A95%3A56%3A6F%3AB6", ``, nil)
	if resp.StatusCode != http.StatusConflict || driver.forgotten != "" {
		t.Fatalf("forget = %d %q", resp.StatusCode, driver.forgotten)
	}
}

func TestPairBluetoothDeviceMapsOperationalErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid address", platform.ErrInvalidBluetoothAddress, http.StatusBadRequest, "invalid_address"},
		{"missing", platform.ErrBluetoothDeviceNotFound, http.StatusNotFound, "bluetooth_device_not_found"},
		{"busy", platform.ErrBluetoothPairInProgress, http.StatusConflict, "pairing_in_progress"},
		{"rejected", platform.ErrBluetoothPairRejected, http.StatusConflict, "pairing_failed"},
		{"timeout", platform.ErrBluetoothPairTimeout, http.StatusGatewayTimeout, "pairing_timeout"},
		{"unavailable", platform.ErrBluetoothUnavailable, http.StatusServiceUnavailable, "bluetooth_unavailable"},
		{"unexpected", errors.New("upstream detail"), http.StatusBadGateway, "bluetooth_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			driver := &pairingStubDriver{pairErr: tt.err}
			ts, _, _, _ := newTestServerWithDriver(t, driver,
				slog.New(slog.NewJSONHandler(&log, nil)))
			resp := request(t, http.MethodPost, ts.URL+"/api/v1/bluetooth/devices/AA%3ABB%3ACC%3ADD%3AEE%3AFF/pair", `{}`, nil)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			var result errorResponse
			json.NewDecoder(resp.Body).Decode(&result)
			if result.Code != tt.code {
				t.Fatalf("response = %#v", result)
			}
			if !strings.Contains(log.String(), "Bluetooth pairing failed") || !strings.Contains(log.String(), "AA:BB:CC:DD:EE:FF") {
				t.Fatalf("pairing error was not recorded by configured logger: %s", log.String())
			}
		})
	}
}

func TestDashboardCanonicalRoutes(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	client := *ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, path := range []string{"/admin", "/admin/"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/operations/jobs" {
			t.Fatalf("%s redirect = %d %q", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	resp, err := client.Get(ts.URL + "/admin/setup/pair")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nested dashboard status = %d", resp.StatusCode)
	}
}

const jobBody = `{"jobId":"o1","template":"test-page","data":{"line":"api"},"printerIds":["cashier"]}`

func request(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestCompositeSubmissionAndLookup(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	resp := request(t, http.MethodPost, ts.URL+"/api/v1/jobs", jobBody, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var first jobs.JobDetail
	json.NewDecoder(resp.Body).Decode(&first)
	if first.UID == "" || len(first.OriginalPrinters) != 1 {
		t.Fatalf("first = %+v", first)
	}
	resp = request(t, http.MethodPost, ts.URL+"/api/v1/jobs", jobBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate status = %d", resp.StatusCode)
	}
	var duplicate jobs.JobDetail
	json.NewDecoder(resp.Body).Decode(&duplicate)
	if !duplicate.Duplicate || duplicate.UID != first.UID {
		t.Fatalf("duplicate = %+v", duplicate)
	}
	resp = request(t, http.MethodGet, ts.URL+"/api/v1/jobs/"+first.UID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	resp = request(t, http.MethodGet, ts.URL+"/api/v1/jobs?jobId=o1", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
}

func TestConflictingSubmissionIs409(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	request(t, http.MethodPost, ts.URL+"/api/v1/jobs", jobBody, nil)
	changed := `{"jobId":"o1","template":"test-page","data":{"line":"changed"},"printerIds":["cashier"]}`
	resp := request(t, http.MethodPost, ts.URL+"/api/v1/jobs", changed, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var result errorResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Code != "idempotency_conflict" {
		t.Fatalf("error = %+v", result)
	}
}

func TestAllowedOriginRequiresToken(t *testing.T) {
	ts, auth, _, _ := newTestServer(t)
	resp := request(t, http.MethodPost, ts.URL+"/api/v1/jobs", jobBody, map[string]string{"Origin": posOrigin})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	code, _, _ := auth.GeneratePairingCode()
	pair := request(t, http.MethodPost, ts.URL+"/api/v1/pair", `{"code":"`+code+`"}`, map[string]string{"Origin": posOrigin})
	var token struct {
		Token string `json:"token"`
	}
	json.NewDecoder(pair.Body).Decode(&token)
	resp = request(t, http.MethodPost, ts.URL+"/api/v1/jobs", jobBody, map[string]string{
		"Origin": posOrigin, "Authorization": "Bearer " + token.Token,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("authenticated status = %d", resp.StatusCode)
	}
}

func TestUnknownOriginAndAdminMutationRejected(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	resp := request(t, http.MethodPost, ts.URL+"/api/v1/jobs", jobBody, map[string]string{"Origin": "https://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POS status = %d", resp.StatusCode)
	}
	resp = request(t, http.MethodPost, ts.URL+"/api/v1/printers/cashier/disable", "", map[string]string{"Origin": posOrigin})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin status = %d", resp.StatusCode)
	}
}

func TestQueueAndRunActions(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	resp := request(t, http.MethodPost, ts.URL+"/api/v1/jobs", jobBody, nil)
	var job jobs.JobDetail
	json.NewDecoder(resp.Body).Decode(&job)
	run := job.OriginalPrinters[0].Runs[0]
	resp = request(t, http.MethodGet, ts.URL+"/api/v1/printers/cashier/queue", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue status = %d", resp.StatusCode)
	}
	resp = request(t, http.MethodPost, ts.URL+"/api/v1/print-runs/"+run.UID+"/cancel", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d", resp.StatusCode)
	}
	reprintBody := `{"reprintRequestId":"ui-1","printerIds":["cashier"],"reason":"damaged"}`
	resp = request(t, http.MethodPost, ts.URL+"/api/v1/jobs/"+job.UID+"/reprint", reprintBody, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reprint status = %d", resp.StatusCode)
	}
	resp = request(t, http.MethodPost, ts.URL+"/api/v1/jobs/"+job.UID+"/reprint", reprintBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate reprint status = %d", resp.StatusCode)
	}
}

func TestSystemLogsAPIBackedFiltersRetentionAndPagination(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "system-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := appLogging.NewStore(db)
	now := time.Now().UTC()
	for _, record := range []appLogging.Record{
		{CreatedAt: now.Add(-time.Minute), Level: "error", Message: "timeout one", PrinterID: "kitchen", RunUID: "run_2", Attributes: map[string]any{"path": "/print"}},
		{CreatedAt: now.Add(-2 * time.Minute), Level: "warn", Message: "timeout two", PrinterID: "kitchen", RunUID: "run_1", Attributes: map[string]any{"path": "/print"}},
		{CreatedAt: now.Add(-appLogging.Retention - time.Minute), Level: "error", Message: "expired", PrinterID: "kitchen", Attributes: map[string]any{}},
	} {
		if err := store.Insert(record); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{logs: store}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/system-logs?levels=warn,error&printerId=kitchen&q=timeout&limit=1", nil)
	server.handleSystemLogs(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var first struct {
		Logs       []appLogging.Record `json:"logs"`
		NextCursor string              `json:"nextCursor"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if len(first.Logs) != 1 || first.Logs[0].RunUID != "run_2" || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/system-logs?levels=warn,error&printerId=kitchen&q=timeout&limit=1&cursor="+first.NextCursor, nil)
	server.handleSystemLogs(recorder, request)
	var second struct {
		Logs []appLogging.Record `json:"logs"`
	}
	json.NewDecoder(recorder.Body).Decode(&second)
	if len(second.Logs) != 1 || second.Logs[0].RunUID != "run_1" {
		t.Fatalf("second page = %#v", second)
	}
}

func TestPrinterLogsAPIFiltersAliasesAndPaginates(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "printer-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	diag := diagnostics.NewService(db, "printer-logs.db")
	now := time.Now().UTC()
	for _, event := range []struct{ typ, printer, run, message string }{
		{"print_run.failed", "kitchen", "run_2", "timeout writing"},
		{"print_run.failed", "kitchen", "run_1", "timeout connecting"},
		{"print_run.failed", "bar", "run_3", "timeout"},
	} {
		if err := diag.PersistEvent(event.typ, event.printer, event.run, event.message, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(-time.Second)
	}
	server := &Server{diag: diag}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/printer-logs?printerId=kitchen&event=print_run_failed&q=timeout&limit=1", nil)
	server.handlePrinterLogs(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Events     []diagnostics.EventRow `json:"events"`
		NextCursor string                 `json:"nextCursor"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].PrinterID != "kitchen" || result.NextCursor == "" {
		t.Fatalf("result = %#v", result)
	}
}
