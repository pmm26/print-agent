package jobs

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/storage"
)

type stubDirectory map[string]config.PrinterConfig

func (d stubDirectory) PrinterExists(id string) bool { _, ok := d[id]; return ok }
func (d stubDirectory) PrinterConfig(id string) (config.PrinterConfig, error) {
	p, ok := d[id]
	if !ok {
		return p, errors.New("printer not found")
	}
	return p, nil
}

func newTestService(t *testing.T) (*Service, *Repository) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := NewRepository(db)
	configRepo := config.NewRepository(db)
	dir := stubDirectory{}
	for _, id := range []string{"cashier", "kitchen", "bar"} {
		cfg := config.PrinterConfig{ID: id, Encoding: "CP858", CharactersPerLine: 32,
			Transport: config.TransportMock, Endpoint: "mock://" + id}
		cfg.ApplyDefaults()
		if err := configRepo.SavePrinter(cfg); err != nil {
			t.Fatal(err)
		}
		dir[id] = cfg
	}
	svc := NewService(repo, events.NewBus(), dir, escpos.NewRenderer(), escpos.KnownTemplate)
	svc.SetPayloadValidator(escpos.ValidateTemplateData)
	return svc, repo
}

func sampleRequest() CreateJobRequest {
	return CreateJobRequest{
		JobID: "order-1256", Template: escpos.TemplateKitchenTicket,
		Data:       json.RawMessage(`{"orderNumber":"1256","items":[{"name":"Soup","quantity":1}]}`),
		PrinterIDs: []string{"kitchen", "bar"}, Source: "test", Owner: "local",
	}
}

func TestAcceptCompositeIdempotency(t *testing.T) {
	svc, repo := newTestService(t)
	first, err := svc.Accept(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Accept(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if second.UID != first.UID || !second.Duplicate {
		t.Fatalf("duplicate = %+v, first = %+v", second, first)
	}
	runs, err := repo.runsWhere(`job_uid = ? ORDER BY run_number`, first.UID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %d, %v", len(runs), err)
	}
}

func TestTargetOrderDoesNotAffectEquality(t *testing.T) {
	svc, _ := newTestService(t)
	first, err := svc.Accept(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	reordered := sampleRequest()
	reordered.PrinterIDs[0], reordered.PrinterIDs[1] = reordered.PrinterIDs[1], reordered.PrinterIDs[0]
	second, err := svc.Accept(reordered)
	if err != nil || !second.Duplicate || second.UID != first.UID {
		t.Fatalf("reordered duplicate = %+v, %v", second, err)
	}
}

func TestConflictingDuplicateAndDifferentTemplate(t *testing.T) {
	svc, _ := newTestService(t)
	first, err := svc.Accept(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	changed := sampleRequest()
	changed.Data = json.RawMessage(`{"orderNumber":"1256","items":[{"name":"Changed","quantity":1}]}`)
	if _, err := svc.Accept(changed); err == nil {
		t.Fatal("changed data did not conflict")
	} else if conflictErr := new(ConflictError); !errors.As(err, &conflictErr) || conflictErr.Code != "idempotency_conflict" {
		t.Fatalf("changed data error = %v", err)
	}
	other := sampleRequest()
	other.Template = escpos.TemplateBarTicket
	second, err := svc.Accept(other)
	if err != nil || second.UID == first.UID {
		t.Fatalf("different template = %+v, %v", second, err)
	}
}

func TestConcurrentSubmissionCreatesOneJob(t *testing.T) {
	svc, repo := newTestService(t)
	const callers = 8
	var wg sync.WaitGroup
	results := make(chan JobDetail, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.Accept(sampleRequest())
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	var uid string
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if uid == "" {
			uid = result.UID
		} else if result.UID != uid {
			t.Fatalf("concurrent UID = %s, want %s", result.UID, uid)
		}
	}
	var count int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("job count = %d, %v", count, err)
	}
}

func TestQueueClaimAndStateTransitions(t *testing.T) {
	svc, repo := newTestService(t)
	accepted, err := svc.Accept(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	run, err := repo.ClaimNextQueued("kitchen")
	if err != nil || run.Status != RunProcessing || run.RunNumber != 1 {
		t.Fatalf("claim = %+v, %v", run, err)
	}
	if _, err := repo.ClaimNextQueued("kitchen"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim = %v", err)
	}
	if err := repo.MarkTransmitted(run.UID, 120); err != nil {
		t.Fatal(err)
	}
	detail, _ := repo.GetDetail(accepted.UID)
	if detail.FulfilledPrinterCount != 1 || detail.State != "queued" {
		t.Fatalf("detail after one printer = %+v", detail)
	}
}

func TestRetryIsSeparateRunCreatedAfterMaterialization(t *testing.T) {
	svc, repo := newTestService(t)
	accepted, _ := svc.Accept(sampleRequest())
	first, _ := repo.ClaimNextQueued("kitchen")
	if err := repo.MarkFailed(first.UID, 0, "not_sent", "offline", true); err != nil {
		t.Fatal(err)
	}
	detail, _ := repo.GetDetail(accepted.UID)
	if !detail.OriginalPrinters[0].RetryPending && !detail.OriginalPrinters[1].RetryPending {
		t.Fatal("safe failure is not retry pending")
	}
	created, err := repo.MaterializeRetries("kitchen", MaxAutomaticRetries,
		func(job Job, target JobPrinter, previous PrintRun, runNumber int) (string, error) {
			return ExpectedContentHash(svc.renderer, job, target, previous.ContentMode, runNumber)
		})
	if err != nil || len(created) != 1 {
		t.Fatalf("retry creation = %+v, %v", created, err)
	}
	if created[0].RunNumber != 2 || created[0].Trigger != TriggerAutomaticRetry || created[0].PreviousRunUID != first.UID {
		t.Fatalf("retry = %+v", created[0])
	}
	again, err := repo.MaterializeRetries("kitchen", MaxAutomaticRetries,
		func(job Job, target JobPrinter, previous PrintRun, runNumber int) (string, error) {
			return "unused", nil
		})
	if err != nil || len(again) != 0 {
		t.Fatalf("duplicate retry = %+v, %v", again, err)
	}
}

func TestRetryLimitCreatesAtMostThreeAutomaticRuns(t *testing.T) {
	svc, repo := newTestService(t)
	req := sampleRequest()
	req.PrinterIDs = []string{"kitchen"}
	accepted, _ := svc.Accept(req)
	run, _ := repo.ClaimNextQueued("kitchen")
	if err := repo.MarkFailed(run.UID, 0, "not_sent", "offline", true); err != nil {
		t.Fatal(err)
	}
	render := func(job Job, target JobPrinter, previous PrintRun, runNumber int) (string, error) {
		return ExpectedContentHash(svc.renderer, job, target, previous.ContentMode, runNumber)
	}
	for retry := 0; retry < MaxAutomaticRetries; retry++ {
		created, err := repo.MaterializeRetries("kitchen", MaxAutomaticRetries, render)
		if err != nil || len(created) != 1 {
			t.Fatalf("retry %d creation = %+v, %v", retry+1, created, err)
		}
		claimed, err := repo.ClaimNextQueued("kitchen")
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.MarkFailed(claimed.UID, 0, "not_sent", "offline", true); err != nil {
			t.Fatal(err)
		}
	}
	created, err := repo.MaterializeRetries("kitchen", MaxAutomaticRetries, render)
	if err != nil || len(created) != 0 {
		t.Fatalf("fourth retry = %+v, %v", created, err)
	}
	detail, _ := repo.GetDetail(accepted.UID)
	runs := detail.OriginalPrinters[0].Runs
	if len(runs) != 4 || runs[len(runs)-1].Retryable {
		t.Fatalf("retry history = %+v", runs)
	}
}

func TestRetryAndManualReprintRaceCreatesOneNextRun(t *testing.T) {
	svc, repo := newTestService(t)
	req := sampleRequest()
	req.PrinterIDs = []string{"kitchen"}
	accepted, _ := svc.Accept(req)
	first, _ := repo.ClaimNextQueued("kitchen")
	repo.MarkFailed(first.UID, 0, "not_sent", "offline", true)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _ = repo.MaterializeRetries("kitchen", MaxAutomaticRetries,
			func(job Job, target JobPrinter, previous PrintRun, runNumber int) (string, error) {
				return ExpectedContentHash(svc.renderer, job, target, previous.ContentMode, runNumber)
			})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _ = svc.Reprint(accepted.UID, ReprintRequest{RequestID: "race", PrinterIDs: []string{"kitchen"}}, "local")
	}()
	close(start)
	wg.Wait()
	detail, err := repo.GetDetail(accepted.UID)
	if err != nil {
		t.Fatal(err)
	}
	runs := detail.OriginalPrinters[0].Runs
	if len(runs) != 2 || runs[1].RunNumber != 2 || runs[1].Status != RunQueued {
		t.Fatalf("race created unsafe history: %+v", runs)
	}
}

func TestQueueSequencePreservesAcceptanceOrder(t *testing.T) {
	svc, repo := newTestService(t)
	firstReq := sampleRequest()
	firstReq.PrinterIDs = []string{"kitchen"}
	first, _ := svc.Accept(firstReq)
	secondReq := firstReq
	secondReq.JobID = "order-1257"
	second, _ := svc.Accept(secondReq)
	claimed, err := repo.ClaimNextQueued("kitchen")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.JobUID != first.UID || claimed.JobUID == second.UID {
		t.Fatalf("first claim = %+v", claimed)
	}
}

func TestManualReprintIdempotencyAndFulfillment(t *testing.T) {
	svc, repo := newTestService(t)
	accepted, _ := svc.Accept(sampleRequest())
	first, _ := repo.ClaimNextQueued("kitchen")
	if err := repo.MarkUncertain(first.UID, 20, "ambiguous_write", "link dropped"); err != nil {
		t.Fatal(err)
	}
	req := ReprintRequest{RequestID: "operator-1", PrinterIDs: []string{"kitchen"}, Reason: "damaged"}
	created, err := svc.Reprint(accepted.UID, req, "local")
	if err != nil || len(created.PrintRuns) != 1 {
		t.Fatalf("reprint = %+v, %v", created, err)
	}
	run := created.PrintRuns[0]
	if run.UID == first.UID || run.Trigger != TriggerManualReprint || run.RunNumber != 2 || run.ReprintOfRunUID != first.UID {
		t.Fatalf("manual run = %+v", run)
	}
	duplicate, err := svc.Reprint(accepted.UID, req, "local")
	if err != nil || !duplicate.Duplicate || duplicate.PrintRuns[0].UID != run.UID {
		t.Fatalf("duplicate reprint = %+v, %v", duplicate, err)
	}
	claimed, _ := repo.ClaimNextQueued("kitchen")
	if err := repo.MarkTransmitted(claimed.UID, 100); err != nil {
		t.Fatal(err)
	}
	detail, _ := repo.GetDetail(accepted.UID)
	if detail.FulfilledPrinterCount != 1 {
		t.Fatalf("manual success did not fulfill target: %+v", detail)
	}
}

func TestMultiPrinterReprintIsAtomicAndConflictsOnChangedBody(t *testing.T) {
	svc, repo := newTestService(t)
	accepted, _ := svc.Accept(sampleRequest())
	for _, printer := range accepted.OriginalPrinters {
		if err := repo.CancelRun(printer.Runs[0].UID); err != nil {
			t.Fatal(err)
		}
	}
	req := ReprintRequest{RequestID: "all-printers", PrinterIDs: []string{"bar", "kitchen"}, Reason: "damaged"}
	result, err := svc.Reprint(accepted.UID, req, "local")
	if err != nil || len(result.PrintRuns) != 2 {
		t.Fatalf("multi reprint = %+v, %v", result, err)
	}
	duplicate, err := svc.Reprint(accepted.UID, req, "local")
	if err != nil || !duplicate.Duplicate || len(duplicate.PrintRuns) != 2 {
		t.Fatalf("multi duplicate = %+v, %v", duplicate, err)
	}
	changed := req
	changed.Reason = "different"
	if _, err := svc.Reprint(accepted.UID, changed, "local"); err == nil {
		t.Fatal("changed reprint body did not conflict")
	}
}

func TestConfirmUncertainFulfillsTarget(t *testing.T) {
	svc, repo := newTestService(t)
	accepted, _ := svc.Accept(sampleRequest())
	run, _ := repo.ClaimNextQueued("kitchen")
	repo.MarkUncertain(run.UID, 10, "ambiguous_write", "unknown")
	if err := svc.ConfirmPrinted(run.UID); err != nil {
		t.Fatal(err)
	}
	detail, _ := repo.GetDetail(accepted.UID)
	if detail.FulfilledPrinterCount != 1 || detail.OriginalPrinters[1].Runs[0].Resolution != ResolutionConfirmedPrinted {
		// Original printer order is request order; locate robustly if it changes.
		found := false
		for _, printer := range detail.OriginalPrinters {
			if printer.PrinterID == "kitchen" && printer.Fulfilled {
				found = true
			}
		}
		if !found {
			t.Fatalf("confirmed uncertain detail = %+v", detail)
		}
	}
}

func TestTargetCancellationAndReactivation(t *testing.T) {
	svc, repo := newTestService(t)
	accepted, _ := svc.Accept(sampleRequest())
	run, _ := repo.ClaimNextQueued("kitchen")
	repo.MarkTransmitted(run.UID, 10)
	if err := svc.CancelJobTargets(accepted.UID, []string{"bar"}, "station closed"); err != nil {
		t.Fatal(err)
	}
	detail, _ := repo.GetDetail(accepted.UID)
	if detail.State != "partially_completed" || !detail.PartiallyFulfilled {
		t.Fatalf("cancelled target aggregate = %+v", detail)
	}
	reprint, err := svc.Reprint(accepted.UID, ReprintRequest{RequestID: "reactivate", PrinterIDs: []string{"bar"}}, "local")
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := repo.ClaimNextQueued("bar")
	if claimed.UID != reprint.PrintRuns[0].UID {
		t.Fatalf("claimed = %+v", claimed)
	}
	repo.MarkTransmitted(claimed.UID, 10)
	detail, _ = repo.GetDetail(accepted.UID)
	if detail.State != "completed" || detail.FulfilledPrinterCount != 2 {
		t.Fatalf("reactivated target aggregate = %+v", detail)
	}
}

func TestRecoverAbandonedIsUncertain(t *testing.T) {
	svc, repo := newTestService(t)
	svc.Accept(sampleRequest())
	run, _ := repo.ClaimNextQueued("kitchen")
	recovered, err := repo.RecoverAbandoned()
	if err != nil || len(recovered) != 1 || recovered[0].UID != run.UID || recovered[0].Status != RunUncertain {
		t.Fatalf("recovered = %+v, %v", recovered, err)
	}
}

func TestPurgeExpiresOnlySettledJobs(t *testing.T) {
	svc, repo := newTestService(t)
	completed, _ := svc.Accept(sampleRequest())
	for _, printer := range []string{"kitchen", "bar"} {
		run, _ := repo.ClaimNextQueued(printer)
		repo.MarkTransmitted(run.UID, 10)
	}
	active := sampleRequest()
	active.JobID = "active"
	activeJob, _ := svc.Accept(active)
	old := ts(time.Now().Add(-time.Hour))
	repo.db.Exec(`UPDATE jobs SET expires_at = ?`, old)
	if err := repo.PurgeExpired(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetJob(completed.UID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed job not purged: %v", err)
	}
	if _, err := repo.GetJob(activeJob.UID); err != nil {
		t.Fatalf("active job purged: %v", err)
	}
}
