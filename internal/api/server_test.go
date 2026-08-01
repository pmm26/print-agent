package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"print-agent/internal/bluetooth"
	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/printers"
	"print-agent/internal/storage"

	"log/slog"
)

const posOrigin = "https://pos.example.com"

type stubConnector struct{}

func (stubConnector) EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (string, error) {
	return cfg.Endpoint, nil
}
func (stubConnector) Disconnect(ctx context.Context, cfg config.PrinterConfig) error { return nil }
func (stubConnector) ListCandidates(ctx context.Context) ([]bluetooth.Candidate, error) {
	return nil, nil
}
func (stubConnector) OpenSystemBluetoothSettings(ctx context.Context) error { return nil }
func (stubConnector) VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error {
	return nil
}

func newTestServer(t *testing.T) (*httptest.Server, *AuthService) {
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
	configRepo.SavePrinter(cfg)

	jobsRepo := jobs.NewRepository(db)
	manager := printers.NewManager(configRepo, jobsRepo, stubConnector{}, bus, nil)
	jobsService := jobs.NewService(jobsRepo, bus, manager, escpos.KnownTemplate)
	jobsService.SetWaker(manager)
	auth := NewAuthService(db)
	diag := diagnostics.NewService(db, "test.db")
	srv := NewServer(jobsService, jobsRepo, manager, configRepo, stubConnector{}, diag, auth, bus,
		slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, auth
}

const jobBody = `{"jobId":"o1","documents":[{"deliveryId":"o1:c","printerId":"cashier","template":"test-page","data":{}}]}`

func post(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestLocalSubmissionWithoutOrigin(t *testing.T) {
	ts, _ := newTestServer(t)
	if resp := post(t, ts.URL+"/api/v1/jobs", jobBody, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	// Duplicate returns 200 with duplicate flag.
	resp := post(t, ts.URL+"/api/v1/jobs", jobBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200", resp.StatusCode)
	}
	var result jobs.PrintJobResult
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Duplicate {
		t.Error("duplicate flag missing")
	}
}

func TestUnknownOriginRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/jobs", jobBody, map[string]string{"Origin": "https://evil.example.com"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAllowedOriginRequiresToken(t *testing.T) {
	ts, auth := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/jobs", jobBody, map[string]string{"Origin": posOrigin})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}

	// Pair to get a token, then the same request succeeds.
	code, _ := auth.GeneratePairingCode()
	pairResp := post(t, ts.URL+"/api/v1/pair", `{"code":"`+code+`"}`, map[string]string{"Origin": posOrigin})
	if pairResp.StatusCode != http.StatusOK {
		t.Fatalf("pair status = %d", pairResp.StatusCode)
	}
	var pair struct{ Token string }
	json.NewDecoder(pairResp.Body).Decode(&pair)

	resp = post(t, ts.URL+"/api/v1/jobs", jobBody, map[string]string{
		"Origin": posOrigin, "Authorization": "Bearer " + pair.Token})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("with-token status = %d, want 202", resp.StatusCode)
	}

	// Pairing codes are one-time.
	if again := post(t, ts.URL+"/api/v1/pair", `{"code":"`+code+`"}`,
		map[string]string{"Origin": posOrigin}); again.StatusCode != http.StatusForbidden {
		t.Fatalf("code reuse status = %d, want 403", again.StatusCode)
	}
}

func TestPreflightHeaders(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/api/v1/jobs", nil)
	req.Header.Set("Origin", posOrigin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != posOrigin {
		t.Errorf("ACAO = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Idempotency-Key") {
		t.Errorf("Idempotency-Key missing from allowed headers: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Errorf("PNA header = %q", got)
	}
}

func TestIdempotencyKeyMismatch(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/jobs", jobBody, map[string]string{"Idempotency-Key": "different"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminEndpointsRejectCrossOrigin(t *testing.T) {
	ts, _ := newTestServer(t)
	// Even the ALLOWED POS origin must not reach management endpoints.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/printers", nil)
	req.Header.Set("Origin", posOrigin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestTokenRevocation(t *testing.T) {
	ts, auth := newTestServer(t)
	code, _ := auth.GeneratePairingCode()
	token, err := auth.Pair(code, posOrigin, "test client")
	if err != nil {
		t.Fatal(err)
	}
	tokens, _ := auth.ListTokens()
	if len(tokens) != 1 {
		t.Fatalf("tokens = %d", len(tokens))
	}
	if err := auth.RevokeToken(tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	resp := post(t, ts.URL+"/api/v1/jobs", jobBody, map[string]string{
		"Origin": posOrigin, "Authorization": "Bearer " + token})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", resp.StatusCode)
	}
}
