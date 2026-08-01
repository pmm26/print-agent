// Package integration exercises the full print pipeline — job service,
// SQLite queue, printer workers, reconnect logic — against mock transports
// with scripted failures. These are the spec §24 acceptance scenarios that
// don't need hardware.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"print-agent/internal/bluetooth"
	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/printers"
	"print-agent/internal/storage"
	"print-agent/internal/transport"
)

// stubConnector mimics the OS view of the Bluetooth link. linkDown
// simulates macOS reporting the paired printer as disconnected while the
// serial endpoint still accepts (buffers) writes.
type stubConnector struct{ linkDown atomic.Bool }

func (c *stubConnector) EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (string, error) {
	return cfg.Endpoint, nil
}
func (c *stubConnector) Disconnect(ctx context.Context, cfg config.PrinterConfig) error { return nil }
func (c *stubConnector) ListCandidates(ctx context.Context) ([]bluetooth.Candidate, error) {
	return nil, nil
}
func (c *stubConnector) OpenSystemBluetoothSettings(ctx context.Context) error { return nil }
func (c *stubConnector) VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error {
	if c.linkDown.Load() {
		return bluetooth.ErrNotConnected
	}
	return nil
}

// harness wires the real components with singleton mock transports.
type harness struct {
	t         *testing.T
	repo      *jobs.Repository
	service   *jobs.Service
	manager   *printers.Manager
	connector *stubConnector

	mu    sync.Mutex
	mocks map[string]*transport.MockTransport
}

func newHarness(t *testing.T, printerIDs ...string) *harness {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	h := &harness{t: t, mocks: map[string]*transport.MockTransport{}, connector: &stubConnector{}}
	factory := func(cfg config.PrinterConfig) transport.Transport {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m, ok := h.mocks[cfg.ID]; ok {
			return m
		}
		m := transport.NewMock("mock://" + cfg.ID)
		h.mocks[cfg.ID] = m
		return m
	}

	configRepo := config.NewRepository(db)
	for _, id := range printerIDs {
		cfg := config.PrinterConfig{ID: id, Enabled: true, Transport: config.TransportMock,
			Endpoint: "mock://" + id, AutoReconnect: true}
		cfg.ApplyDefaults()
		if err := configRepo.SavePrinter(cfg); err != nil {
			t.Fatal(err)
		}
	}

	bus := events.NewBus()
	h.repo = jobs.NewRepository(db)
	h.manager = printers.NewManager(configRepo, h.repo, h.connector, bus, factory)
	h.service = jobs.NewService(h.repo, bus, h.manager, escpos.KnownTemplate)
	h.service.SetWaker(h.manager)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.manager.Stop() })
	if err := h.manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) mock(printerID string) *transport.MockTransport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.mocks[printerID]; ok {
		return m
	}
	m := transport.NewMock("mock://" + printerID)
	h.mocks[printerID] = m
	return m
}

func (h *harness) submit(jobID string, printerIDs ...string) {
	h.t.Helper()
	docs := make([]jobs.DocumentRequest, len(printerIDs))
	for i, p := range printerIDs {
		docs[i] = jobs.DocumentRequest{
			DeliveryID: jobID + ":" + p,
			PrinterID:  p,
			Template:   escpos.TemplateTestPage,
			Data:       json.RawMessage(`{"line":"integration"}`),
		}
	}
	if _, err := h.service.Accept(jobs.CreatePrintJobRequest{JobID: jobID, Source: "test", Documents: docs}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) waitStatus(deliveryID string, want jobs.DeliveryStatus, timeout time.Duration) jobs.Delivery {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last jobs.Delivery
	for time.Now().Before(deadline) {
		d, err := h.repo.GetDeliveryByExternalID(deliveryID)
		if err == nil {
			last = d
			if d.Status == want {
				return d
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("delivery %s did not reach %s (last: %s / %q)", deliveryID, want, last.Status, last.LastError)
	return last
}

func TestHappyPathTransmits(t *testing.T) {
	h := newHarness(t, "cashier", "kitchen", "bar")
	h.submit("order-1", "cashier", "kitchen", "bar")
	for _, p := range []string{"cashier", "kitchen", "bar"} {
		d := h.waitStatus("order-1:"+p, jobs.DeliveryTransmitted, 3*time.Second)
		if d.BytesWritten == 0 {
			t.Errorf("%s: bytesWritten = 0", p)
		}
	}
	if len(h.mock("kitchen").Writes()) != 1 {
		t.Errorf("kitchen writes = %d, want 1", len(h.mock("kitchen").Writes()))
	}
}

func TestOfflinePrinterDoesNotBlockOthers(t *testing.T) {
	h := newHarness(t, "cashier", "kitchen")
	// Kitchen is "powered off": connects fail after the initial connection.
	kitchen := h.mock("kitchen")
	kitchen.FailConnect = errors.New("device unreachable")
	kitchen.Close()

	h.submit("order-2", "cashier", "kitchen")

	// Cashier prints normally.
	h.waitStatus("order-2:cashier", jobs.DeliveryTransmitted, 3*time.Second)
	// Kitchen delivery stays queued (worker can't connect, nothing was sent).
	d, err := h.repo.GetDeliveryByExternalID("order-2:kitchen")
	if err != nil || d.Status != jobs.DeliveryQueued {
		t.Fatalf("kitchen delivery = %s (%v), want queued", d.Status, err)
	}

	// "Power the printer back on" and force an immediate reconnect.
	kitchen.FailConnect = nil
	h.manager.Reconnect("kitchen")
	h.waitStatus("order-2:kitchen", jobs.DeliveryTransmitted, 5*time.Second)
}

func TestMidWriteFailureBecomesUncertain(t *testing.T) {
	h := newHarness(t, "bar")
	h.mock("bar").FailNextWrite(errors.New("connection reset"), 40)

	h.submit("order-3", "bar")
	d := h.waitStatus("order-3:bar", jobs.DeliveryUncertain, 3*time.Second)
	if d.BytesWritten != 40 {
		t.Errorf("bytesWritten = %d, want 40", d.BytesWritten)
	}

	// Uncertain deliveries must NOT retry automatically.
	time.Sleep(300 * time.Millisecond)
	d, _ = h.repo.GetDeliveryByExternalID("order-3:bar")
	if d.Status != jobs.DeliveryUncertain {
		t.Fatalf("uncertain delivery was auto-retried: %s", d.Status)
	}

	// A manual reprint prints a fresh delivery with the marker.
	reprint, err := h.service.Reprint("order-3:bar")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(reprint.ExternalDeliveryID, jobs.DeliveryTransmitted, 3*time.Second)
	writes := h.mock("bar").Writes()
	last := writes[len(writes)-1]
	if !containsBytes(last, []byte("*** REPRINT ***")) {
		t.Error("reprint output missing marker")
	}
}

func TestCleanWriteFailureRetriesAutomatically(t *testing.T) {
	h := newHarness(t, "cashier")
	// First write fails before any byte is accepted — retry is safe.
	h.mock("cashier").FailNextWrite(errors.New("endpoint gone"), 0)

	h.submit("order-4", "cashier")
	d := h.waitStatus("order-4:cashier", jobs.DeliveryTransmitted, 5*time.Second)
	if d.AttemptCount < 2 {
		t.Errorf("attemptCount = %d, want >= 2 (one failure + one retry)", d.AttemptCount)
	}
}

func TestDuplicateSubmissionPrintsOnce(t *testing.T) {
	h := newHarness(t, "cashier")
	h.submit("order-5", "cashier")
	h.waitStatus("order-5:cashier", jobs.DeliveryTransmitted, 3*time.Second)

	// Browser retry after timeout: same job again.
	res, err := h.service.Accept(jobs.CreatePrintJobRequest{
		JobID: "order-5", Source: "test",
		Documents: []jobs.DocumentRequest{{
			DeliveryID: "order-5:cashier", PrinterID: "cashier",
			Template: escpos.TemplateTestPage, Data: json.RawMessage(`{}`),
		}},
	})
	if err != nil || !res.Duplicate {
		t.Fatalf("expected duplicate result, got %+v (%v)", res, err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(h.mock("cashier").Writes()); n != 1 {
		t.Fatalf("printed %d times, want exactly 1", n)
	}
}

// TestLinkDownBeforeWriteStaysQueued: the OS reports the printer
// disconnected before anything is claimed — the delivery must wait in the
// queue (retry-safe) while the worker reconnects.
func TestLinkDownBeforeWriteStaysQueued(t *testing.T) {
	h := newHarness(t, "kitchen")
	h.submit("warmup", "kitchen")
	h.waitStatus("warmup:kitchen", jobs.DeliveryTransmitted, 3*time.Second)

	h.connector.linkDown.Store(true)
	h.submit("order-6", "kitchen")
	time.Sleep(500 * time.Millisecond)
	d, err := h.repo.GetDeliveryByExternalID("order-6:kitchen")
	if err != nil || d.Status != jobs.DeliveryQueued {
		t.Fatalf("delivery = %s (%v), want queued while link is down", d.Status, err)
	}

	// Link restored: the queued delivery prints without operator action.
	h.connector.linkDown.Store(false)
	h.manager.Reconnect("kitchen")
	h.waitStatus("order-6:kitchen", jobs.DeliveryTransmitted, 10*time.Second)
}

// TestSilentBufferedWriteBecomesUncertain covers the macOS trap: the serial
// write "succeeds" (the OS buffers it) but the Bluetooth link died mid-
// flight. The delivery must become uncertain, never transmitted.
func TestSilentBufferedWriteBecomesUncertain(t *testing.T) {
	h := newHarness(t, "kitchen")
	// Drop the link at the exact moment the bytes are "accepted".
	h.mock("kitchen").OnWrite = func([]byte) { h.connector.linkDown.Store(true) }

	h.submit("order-7", "kitchen")
	d := h.waitStatus("order-7:kitchen", jobs.DeliveryUncertain, 5*time.Second)
	if d.BytesWritten == 0 {
		t.Error("bytes were written to the OS; bytesWritten should reflect that")
	}

	// Uncertain must not auto-retry even after the link returns.
	h.mock("kitchen").OnWrite = nil
	h.connector.linkDown.Store(false)
	h.manager.Reconnect("kitchen")
	time.Sleep(500 * time.Millisecond)
	if d, _ := h.repo.GetDeliveryByExternalID("order-7:kitchen"); d.Status != jobs.DeliveryUncertain {
		t.Fatalf("uncertain delivery auto-retried: %s", d.Status)
	}

	// A manual reprint goes through.
	reprint, err := h.service.Reprint("order-7:kitchen")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(reprint.ExternalDeliveryID, jobs.DeliveryTransmitted, 10*time.Second)
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
