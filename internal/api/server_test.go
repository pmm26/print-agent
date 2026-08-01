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

	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/platform"
	"print-agent/internal/printers"
	"print-agent/internal/storage"
)

const posOrigin = "https://pos.example.com"

type stubDriver struct{ platform.UnimplementedDriver }

type pairingStubDriver struct {
	stubDriver
	result  platform.BluetoothDevice
	pairErr error
	address string
	pin     string
}

func (d *pairingStubDriver) ListBluetoothDevices(context.Context) ([]platform.BluetoothDevice, error) {
	return []platform.BluetoothDevice{d.result}, nil
}

func (d *pairingStubDriver) StartBluetoothDiscovery(context.Context) error { return nil }
func (d *pairingStubDriver) StopBluetoothDiscovery(context.Context) error  { return nil }
func (d *pairingStubDriver) PairBluetoothDevice(_ context.Context, address, pin string) (platform.BluetoothDevice, error) {
	d.address = address
	d.pin = pin
	return d.result, d.pairErr
}

func newTestServer(t *testing.T) (*httptest.Server, *AuthService, *jobs.Repository) {
	return newTestServerWithDriver(t, stubDriver{}, slog.New(slog.DiscardHandler))
}

func newTestServerWithDriver(t *testing.T, driver platform.Driver, logger *slog.Logger) (*httptest.Server, *AuthService, *jobs.Repository) {
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
	server := NewServer(svc, repo, manager, configRepo, driver, diag, auth, bus, logger)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts, auth, repo
}

func TestPairBluetoothDeviceReturnsTruthfulReadyDevice(t *testing.T) {
	driver := &pairingStubDriver{result: platform.BluetoothDevice{
		Name: "BlueTooth Printer", Address: "5A:4A:95:56:6F:B6", Connected: true,
		IsPrinter: true, Endpoint: "ble://5A:4A:95:56:6F:B6",
	}}
	ts, _, _ := newTestServerWithDriver(t, driver, slog.New(slog.DiscardHandler))
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
			ts, _, _ := newTestServerWithDriver(t, driver,
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
	ts, _, _ := newTestServer(t)
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
	ts, _, _ := newTestServer(t)
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
	ts, _, _ := newTestServer(t)
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
	ts, auth, _ := newTestServer(t)
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
	ts, _, _ := newTestServer(t)
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
	ts, _, _ := newTestServer(t)
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
