package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/platform"
	"print-agent/internal/printers"
	"print-agent/internal/storage"
	"print-agent/internal/transport"
)

type stubDriver struct {
	platform.UnimplementedDriver
	linkDown atomic.Bool
}

func (d *stubDriver) VerifyConnected(context.Context, config.PrinterConfig) error {
	if d.linkDown.Load() {
		return platform.ErrNotConnected
	}
	return nil
}

type harness struct {
	t       *testing.T
	repo    *jobs.Repository
	service *jobs.Service
	manager *printers.Manager
	driver  *stubDriver
	mu      sync.Mutex
	mocks   map[string]*transport.MockTransport
}

func newHarness(t *testing.T, printerIDs ...string) *harness {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{t: t, mocks: map[string]*transport.MockTransport{}, driver: &stubDriver{}}
	factory := func(cfg config.PrinterConfig) transport.Transport {
		h.mu.Lock()
		defer h.mu.Unlock()
		if mock := h.mocks[cfg.ID]; mock != nil {
			return mock
		}
		mock := transport.NewMock("mock://" + cfg.ID)
		h.mocks[cfg.ID] = mock
		return mock
	}
	configRepo := config.NewRepository(db)
	for _, id := range printerIDs {
		cfg := config.PrinterConfig{ID: id, Enabled: true, Transport: config.TransportBluetoothSerial,
			Endpoint: "mock://" + id, AutoReconnect: true}
		cfg.ApplyDefaults()
		if err := configRepo.SavePrinter(cfg); err != nil {
			t.Fatal(err)
		}
	}
	bus := events.NewBus()
	h.repo = jobs.NewRepository(db)
	h.manager = printers.NewManager(configRepo, h.repo, h.driver, bus, factory)
	h.service = jobs.NewService(h.repo, bus, h.manager, escpos.NewRenderer(), escpos.KnownTemplate)
	h.service.SetWaker(h.manager)
	h.service.SetPayloadValidator(escpos.ValidateTemplateData)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.manager.Stop() })
	if err := h.manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) mock(id string) *transport.MockTransport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if mock := h.mocks[id]; mock != nil {
		return mock
	}
	mock := transport.NewMock("mock://" + id)
	h.mocks[id] = mock
	return mock
}

func (h *harness) submit(jobID string, printerIDs ...string) jobs.JobDetail {
	h.t.Helper()
	result, err := h.service.Accept(jobs.CreateJobRequest{
		JobID: jobID, Template: escpos.TemplateKitchenTicket, PrinterIDs: printerIDs,
		Data:   json.RawMessage(`{"orderNumber":"42","items":[{"name":"Soup","quantity":1}]}`),
		Source: "test", Owner: "local",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *harness) waitRun(uid string, want jobs.PrintRunStatus, timeout time.Duration) jobs.PrintRun {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last jobs.PrintRun
	for time.Now().Before(deadline) {
		run, err := h.repo.GetRun(uid)
		if err == nil {
			last = run
			if run.Status == want {
				return run
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("Run %s did not reach %s; last=%+v", uid, want, last)
	return last
}

func initialRun(job jobs.JobDetail, printerID string) jobs.PrintRun {
	for _, printer := range job.OriginalPrinters {
		if printer.PrinterID == printerID {
			return printer.Runs[0]
		}
	}
	return jobs.PrintRun{}
}

func TestMultiplePrintersFulfillOneJob(t *testing.T) {
	h := newHarness(t, "cashier", "kitchen")
	job := h.submit("multi", "cashier", "kitchen")
	h.waitRun(initialRun(job, "cashier").UID, jobs.RunTransmitted, 4*time.Second)
	h.waitRun(initialRun(job, "kitchen").UID, jobs.RunTransmitted, 4*time.Second)
	detail, err := h.service.Get(job.UID, "local")
	if err != nil || detail.State != "completed" || detail.FulfilledPrinterCount != 2 {
		t.Fatalf("detail = %+v, %v", detail, err)
	}
}

func TestOfflinePrinterDoesNotCreateRuns(t *testing.T) {
	h := newHarness(t, "kitchen")
	h.driver.linkDown.Store(true)
	job := h.submit("offline", "kitchen")
	time.Sleep(300 * time.Millisecond)
	run, _ := h.repo.GetRun(initialRun(job, "kitchen").UID)
	if run.Status != jobs.RunQueued {
		t.Fatalf("offline Run = %+v", run)
	}
	detail, _ := h.service.Get(job.UID, "local")
	if len(detail.OriginalPrinters[0].Runs) != 1 {
		t.Fatalf("offline created extra Runs: %+v", detail)
	}
	h.driver.linkDown.Store(false)
	h.manager.Reconnect("kitchen")
	h.waitRun(run.UID, jobs.RunTransmitted, 5*time.Second)
}

func TestSafeFailureCreatesNewRunOnlyAfterReconnect(t *testing.T) {
	h := newHarness(t, "kitchen")
	mock := h.mock("kitchen")
	mock.FailNextWrite(errors.New("not connected"), 0)
	job := h.submit("safe-retry", "kitchen")
	first := h.waitRun(initialRun(job, "kitchen").UID, jobs.RunFailed, 4*time.Second)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		detail, _ := h.service.Get(job.UID, "local")
		runs := detail.OriginalPrinters[0].Runs
		if len(runs) == 2 && runs[1].Status == jobs.RunTransmitted {
			if runs[1].Trigger != jobs.TriggerAutomaticRetry || runs[1].PreviousRunUID != first.UID {
				t.Fatalf("retry = %+v", runs[1])
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("automatic retry did not transmit")
}

func TestAmbiguousWriteNeverRetries(t *testing.T) {
	h := newHarness(t, "kitchen")
	mock := h.mock("kitchen")
	mock.FailNextWrite(errors.New("link dropped"), 8)
	job := h.submit("uncertain", "kitchen")
	run := h.waitRun(initialRun(job, "kitchen").UID, jobs.RunUncertain, 4*time.Second)
	if run.BytesAccepted != 8 || run.Retryable {
		t.Fatalf("uncertain = %+v", run)
	}
	time.Sleep(2500 * time.Millisecond)
	detail, _ := h.service.Get(job.UID, "local")
	if len(detail.OriginalPrinters[0].Runs) != 1 {
		t.Fatalf("uncertain auto-retried: %+v", detail)
	}
}

func TestPostWriteLinkLossIsUncertain(t *testing.T) {
	h := newHarness(t, "kitchen")
	mock := h.mock("kitchen")
	mock.SetOnWrite(func([]byte) { h.driver.linkDown.Store(true) })
	job := h.submit("link-loss", "kitchen")
	h.waitRun(initialRun(job, "kitchen").UID, jobs.RunUncertain, 4*time.Second)
	detail, _ := h.service.Get(job.UID, "local")
	if !detail.HasUncertainResult || detail.State != "attention_required" {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestManualReprintMarkerAndFulfillment(t *testing.T) {
	h := newHarness(t, "kitchen")
	mock := h.mock("kitchen")
	mock.FailNextWrite(errors.New("partial"), 5)
	job := h.submit("manual", "kitchen")
	h.waitRun(initialRun(job, "kitchen").UID, jobs.RunUncertain, 4*time.Second)
	result, err := h.service.Reprint(job.UID, jobs.ReprintRequest{
		RequestID: "operator-1", PrinterIDs: []string{"kitchen"}, Reason: "damaged",
	}, "local")
	if err != nil {
		t.Fatal(err)
	}
	h.waitRun(result.PrintRuns[0].UID, jobs.RunTransmitted, 5*time.Second)
	writes := mock.Writes()
	if len(writes) < 2 || !bytes.Contains(writes[len(writes)-1], []byte("REPRINT - RUN 2")) {
		t.Fatalf("reprint marker missing from %q", writes[len(writes)-1])
	}
	detail, _ := h.service.Get(job.UID, "local")
	if detail.State != "completed" || !detail.HasManualReprints {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestDisableAndRetirementPreserveQueuedWork(t *testing.T) {
	h := newHarness(t, "kitchen")
	h.driver.linkDown.Store(true)
	job := h.submit("disabled", "kitchen")
	runUID := initialRun(job, "kitchen").UID
	time.Sleep(200 * time.Millisecond)
	if err := h.manager.SetEnabled("kitchen", false); err != nil {
		t.Fatal(err)
	}
	run, _ := h.repo.GetRun(runUID)
	if run.Status != jobs.RunQueued {
		t.Fatalf("disabled Run = %+v", run)
	}
	if err := h.manager.RemovePrinter("kitchen"); err == nil {
		t.Fatal("retired printer with active Run")
	}
	if err := h.service.CancelJobTargets(job.UID, []string{"kitchen"}, "station removed"); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.RemovePrinter("kitchen"); err != nil {
		t.Fatal(err)
	}
}

func TestProcessingRunCannotBeCancelled(t *testing.T) {
	h := newHarness(t, "kitchen")
	mock := h.mock("kitchen")
	mock.HangOnWrite = true
	job := h.submit("processing-cancel", "kitchen")
	runUID := initialRun(job, "kitchen").UID
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, _ := h.repo.GetRun(runUID)
		if run.Status == jobs.RunProcessing {
			if err := h.service.CancelRun(runUID); err == nil {
				t.Fatal("processing Run was cancelled")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Run never reached processing")
}
