package jobs

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"print-agent/internal/events"
	"print-agent/internal/storage"
)

type stubDirectory map[string]bool

func (d stubDirectory) PrinterExists(id string) bool { return d[id] }

func newTestService(t *testing.T) (*Service, *Repository) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := NewRepository(db)
	dir := stubDirectory{"cashier": true, "kitchen": true, "bar": true}
	templates := func(name string) bool { return name != "bogus" }
	return NewService(repo, events.NewBus(), dir, templates), repo
}

func sampleRequest() CreatePrintJobRequest {
	return CreatePrintJobRequest{
		JobID:  "store-001:order-1256",
		Source: "test",
		Documents: []DocumentRequest{
			{DeliveryID: "store-001:order-1256:cashier", PrinterID: "cashier",
				Template: "customer-receipt", Data: json.RawMessage(`{"orderNumber":"1256"}`)},
			{DeliveryID: "store-001:order-1256:kitchen", PrinterID: "kitchen",
				Template: "kitchen-ticket", Data: json.RawMessage(`{"orderNumber":"1256"}`)},
		},
	}
}

func TestAcceptIsIdempotent(t *testing.T) {
	svc, repo := newTestService(t)

	first, err := svc.Accept(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate || len(first.Deliveries) != 2 || first.Status != "queued" {
		t.Fatalf("unexpected first result: %+v", first)
	}

	second, err := svc.Accept(sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate {
		t.Fatal("resubmission must be flagged duplicate")
	}
	if len(second.Deliveries) != 2 {
		t.Fatalf("duplicate created deliveries: %d", len(second.Deliveries))
	}
	all, _ := repo.ListDeliveries("", "", 100)
	if len(all) != 2 {
		t.Fatalf("expected 2 rows in DB, got %d", len(all))
	}
}

func TestAcceptValidation(t *testing.T) {
	svc, _ := newTestService(t)
	cases := []func(*CreatePrintJobRequest){
		func(r *CreatePrintJobRequest) { r.JobID = "" },
		func(r *CreatePrintJobRequest) { r.Documents = nil },
		func(r *CreatePrintJobRequest) { r.Documents[0].PrinterID = "office" },
		func(r *CreatePrintJobRequest) { r.Documents[0].Template = "bogus" },
		func(r *CreatePrintJobRequest) { r.Documents[0].DeliveryID = "" },
		func(r *CreatePrintJobRequest) { r.Documents[1].DeliveryID = r.Documents[0].DeliveryID },
	}
	for i, mutate := range cases {
		req := sampleRequest()
		mutate(&req)
		_, err := svc.Accept(req)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("case %d: want ValidationError, got %v", i, err)
		}
	}
}

func TestDeliveryIDCollisionAcrossJobs(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Accept(sampleRequest()); err != nil {
		t.Fatal(err)
	}
	req := sampleRequest()
	req.JobID = "store-001:order-9999" // new job, reused deliveryId
	req.Documents = req.Documents[:1]
	_, err := svc.Accept(req)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("cross-job deliveryId collision must be rejected, got %v", err)
	}
}

func TestClaimExclusivityAndOrder(t *testing.T) {
	svc, repo := newTestService(t)
	if _, err := svc.Accept(sampleRequest()); err != nil {
		t.Fatal(err)
	}

	first, err := repo.ClaimNextQueued("cashier")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != DeliveryProcessing || first.AttemptCount != 1 {
		t.Fatalf("claimed delivery state: %+v", first)
	}
	// Same printer again: nothing left.
	if _, err := repo.ClaimNextQueued("cashier"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim must find nothing, got %v", err)
	}
	// Other printer's queue is independent.
	if _, err := repo.ClaimNextQueued("kitchen"); err != nil {
		t.Fatalf("kitchen claim: %v", err)
	}
}

func TestReprintCreatesNewDelivery(t *testing.T) {
	svc, repo := newTestService(t)
	svc.Accept(sampleRequest())
	d, _ := repo.ClaimNextQueued("kitchen")
	repo.MarkUncertain(d.ID, 100, "link dropped")

	reprint, err := svc.Reprint(d.ExternalDeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if reprint.ExternalDeliveryID != d.ExternalDeliveryID+":reprint-1" {
		t.Errorf("reprint id = %s", reprint.ExternalDeliveryID)
	}
	if reprint.ReprintOf != d.ExternalDeliveryID || reprint.Status != DeliveryQueued {
		t.Errorf("reprint fields: %+v", reprint)
	}
	// Original is resolved by the reprint.
	orig, _ := repo.GetDeliveryByExternalID(d.ExternalDeliveryID)
	if orig.ResolvedAt == nil {
		t.Error("original should be resolved after reprint")
	}
	// A second reprint gets a distinct ID.
	second, err := svc.Reprint(d.ExternalDeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ExternalDeliveryID != d.ExternalDeliveryID+":reprint-2" {
		t.Errorf("second reprint id = %s", second.ExternalDeliveryID)
	}
}

func TestCancelRules(t *testing.T) {
	svc, repo := newTestService(t)
	svc.Accept(sampleRequest())

	if err := svc.Cancel("store-001:order-1256:cashier"); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	d, _ := repo.ClaimNextQueued("kitchen")
	if err := svc.Cancel(d.ExternalDeliveryID); err == nil {
		t.Fatal("cancelling a processing delivery must fail")
	}
	repo.MarkTransmitted(d.ID, 10)
	if err := svc.Cancel(d.ExternalDeliveryID); err == nil {
		t.Fatal("cancelling a transmitted delivery must fail")
	}
}

func TestRecoverAbandoned(t *testing.T) {
	svc, repo := newTestService(t)
	svc.Accept(sampleRequest())
	d, _ := repo.ClaimNextQueued("cashier")

	ids, err := repo.RecoverAbandoned()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != d.ExternalDeliveryID {
		t.Fatalf("recovered = %v", ids)
	}
	got, _ := repo.GetDeliveryByExternalID(d.ExternalDeliveryID)
	if got.Status != DeliveryUncertain {
		t.Errorf("status after recovery = %s, want uncertain", got.Status)
	}
}

func TestJobStatusAggregation(t *testing.T) {
	mk := func(statuses ...DeliveryStatus) []Delivery {
		out := make([]Delivery, len(statuses))
		for i, s := range statuses {
			out[i] = Delivery{Status: s}
		}
		return out
	}
	cases := []struct {
		in   []Delivery
		want string
	}{
		{mk(DeliveryQueued, DeliveryQueued), "queued"},
		{mk(DeliveryProcessing, DeliveryQueued), "processing"},
		{mk(DeliveryTransmitted, DeliveryTransmitted), "completed"},
		{mk(DeliveryTransmitted, DeliveryCancelled), "completed"},
		{mk(DeliveryTransmitted, DeliveryFailed), "attention"},
		{mk(DeliveryUncertain, DeliveryQueued), "attention"},
	}
	for i, c := range cases {
		if got := JobStatus(c.in); got != c.want {
			t.Errorf("case %d: JobStatus = %s, want %s", i, got, c.want)
		}
	}
	resolved := mk(DeliveryFailed, DeliveryTransmitted)
	now := resolved[0].CreatedAt
	resolved[0].ResolvedAt = &now
	if got := JobStatus(resolved); got != "completed" {
		t.Errorf("resolved failure should read completed, got %s", got)
	}
}
