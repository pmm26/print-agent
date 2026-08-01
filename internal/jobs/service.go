package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"print-agent/internal/events"
)

// maxPayloadBytes bounds a single document's data blob. The HTTP layer also
// enforces a whole-request limit.
const maxPayloadBytes = 64 * 1024

// Waker is implemented by the printer manager; Accept uses it to nudge the
// worker after the transaction commits. SQLite remains the source of truth —
// a missed wake is recovered by the worker's periodic queue poll.
type Waker interface {
	Wake(printerID string)
}

// PrinterDirectory exposes the printer IDs Accept validates against.
type PrinterDirectory interface {
	PrinterExists(id string) bool
}

// TemplateValidator reports whether a template name is renderable.
type TemplateValidator func(name string) bool

type Service struct {
	repo      *Repository
	bus       *events.Bus
	waker     Waker
	printers  PrinterDirectory
	templates TemplateValidator
}

func NewService(repo *Repository, bus *events.Bus, printers PrinterDirectory, templates TemplateValidator) *Service {
	return &Service{repo: repo, bus: bus, printers: printers, templates: templates}
}

// SetWaker wires the printer manager after construction (the manager also
// depends on the repository, so it is created second).
func (s *Service) SetWaker(w Waker) { s.waker = w }

// CreatePrintJobRequest is the POST /api/v1/jobs body.
type CreatePrintJobRequest struct {
	JobID     string            `json:"jobId"`
	Source    string            `json:"-"`
	Documents []DocumentRequest `json:"documents"`
}

type DocumentRequest struct {
	DeliveryID string          `json:"deliveryId"`
	PrinterID  string          `json:"printerId"`
	Template   string          `json:"template"`
	Data       json.RawMessage `json:"data"`
}

// PrintJobResult is the API response for job submission and status.
type PrintJobResult struct {
	JobID      string     `json:"jobId"`
	Accepted   bool       `json:"accepted"`
	Duplicate  bool       `json:"duplicate,omitempty"`
	Status     string     `json:"status"`
	Deliveries []Delivery `json:"deliveries"`
}

// ValidationError distinguishes 4xx-worthy rejections from internal errors.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

// NewValidationError lets other layers (API decoding) produce 400-mapped errors.
func NewValidationError(msg string) *ValidationError { return &ValidationError{msg: msg} }

func validationErrorf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// Accept validates and persists a job. Resubmitting the same jobId returns
// the existing state without creating or printing anything (idempotency).
func (s *Service) Accept(req CreatePrintJobRequest) (PrintJobResult, error) {
	if err := s.validate(req); err != nil {
		return PrintJobResult{}, err
	}

	// Fast path for duplicates; the UNIQUE constraint below closes the race.
	if existing, deliveries, err := s.repo.GetJobByExternalID(req.JobID); err == nil {
		return duplicateResult(existing, deliveries), nil
	} else if !errors.Is(err, ErrNotFound) {
		return PrintJobResult{}, err
	}

	now := time.Now().UTC()
	job := Job{ID: newID(), ExternalJobID: req.JobID, Source: req.Source, CreatedAt: now, AcceptedAt: now}
	deliveries := make([]Delivery, 0, len(req.Documents))
	for _, doc := range req.Documents {
		deliveries = append(deliveries, Delivery{
			ID:                 newID(),
			ExternalDeliveryID: doc.DeliveryID,
			JobID:              job.ID,
			PrinterID:          doc.PrinterID,
			Template:           doc.Template,
			PayloadJSON:        doc.Data,
			Status:             DeliveryQueued,
			CreatedAt:          now,
		})
	}

	if err := s.repo.InsertJobWithDeliveries(job, deliveries); err != nil {
		if isUniqueViolation(err) {
			// Lost a race with an identical concurrent request, or a
			// deliveryId collides with another job's. Return the existing
			// job if there is one; otherwise surface the conflict.
			if existing, existingDeliveries, lookupErr := s.repo.GetJobByExternalID(req.JobID); lookupErr == nil {
				return duplicateResult(existing, existingDeliveries), nil
			}
			return PrintJobResult{}, validationErrorf("a deliveryId in this request already belongs to another job")
		}
		return PrintJobResult{}, err
	}

	s.bus.Publish(events.Event{Type: events.JobAccepted, Message: req.JobID})
	for _, d := range deliveries {
		s.bus.Publish(events.Event{Type: events.DeliveryQueued, PrinterID: d.PrinterID, DeliveryID: d.ExternalDeliveryID})
		if s.waker != nil {
			s.waker.Wake(d.PrinterID)
		}
	}
	return PrintJobResult{JobID: job.ExternalJobID, Accepted: true, Status: JobStatus(deliveries), Deliveries: deliveries}, nil
}

func duplicateResult(job Job, deliveries []Delivery) PrintJobResult {
	return PrintJobResult{
		JobID:      job.ExternalJobID,
		Accepted:   true,
		Duplicate:  true,
		Status:     JobStatus(deliveries),
		Deliveries: deliveries,
	}
}

func (s *Service) validate(req CreatePrintJobRequest) error {
	if req.JobID == "" {
		return validationErrorf("jobId is required")
	}
	if len(req.JobID) > 200 {
		return validationErrorf("jobId too long")
	}
	if len(req.Documents) == 0 {
		return validationErrorf("documents must not be empty")
	}
	if len(req.Documents) > 20 {
		return validationErrorf("too many documents in one job")
	}
	seen := map[string]bool{}
	for i, doc := range req.Documents {
		switch {
		case doc.DeliveryID == "":
			return validationErrorf("documents[%d].deliveryId is required", i)
		case len(doc.DeliveryID) > 250:
			return validationErrorf("documents[%d].deliveryId too long", i)
		case seen[doc.DeliveryID]:
			return validationErrorf("duplicate deliveryId %q within request", doc.DeliveryID)
		case doc.PrinterID == "":
			return validationErrorf("documents[%d].printerId is required", i)
		case !s.printers.PrinterExists(doc.PrinterID):
			return validationErrorf("unknown printerId %q", doc.PrinterID)
		case !s.templates(doc.Template):
			return validationErrorf("unknown template %q", doc.Template)
		case len(doc.Data) > maxPayloadBytes:
			return validationErrorf("documents[%d].data exceeds %d bytes", i, maxPayloadBytes)
		}
		seen[doc.DeliveryID] = true
	}
	return nil
}

// Get returns the job's current state by external ID.
func (s *Service) Get(externalJobID string) (PrintJobResult, error) {
	job, deliveries, err := s.repo.GetJobByExternalID(externalJobID)
	if err != nil {
		return PrintJobResult{}, err
	}
	return PrintJobResult{JobID: job.ExternalJobID, Accepted: true, Status: JobStatus(deliveries), Deliveries: deliveries}, nil
}

// Reprint creates a fresh delivery duplicating the original document. The
// new delivery carries a reprint marker and a derived unique ID, satisfying
// the external_delivery_id UNIQUE constraint.
func (s *Service) Reprint(externalDeliveryID string) (Delivery, error) {
	orig, err := s.repo.GetDeliveryByExternalID(externalDeliveryID)
	if err != nil {
		return Delivery{}, err
	}
	n, err := s.repo.CountReprints(orig.ExternalDeliveryID)
	if err != nil {
		return Delivery{}, err
	}
	now := time.Now().UTC()
	reprint := Delivery{
		ID:                 newID(),
		ExternalDeliveryID: fmt.Sprintf("%s:reprint-%d", orig.ExternalDeliveryID, n+1),
		JobID:              orig.JobID,
		PrinterID:          orig.PrinterID,
		Template:           orig.Template,
		PayloadJSON:        orig.PayloadJSON,
		Status:             DeliveryQueued,
		ReprintOf:          orig.ExternalDeliveryID,
		CreatedAt:          now,
	}
	if err := s.repo.InsertDelivery(reprint); err != nil {
		return Delivery{}, err
	}
	// A reprint acknowledges the original problem.
	if orig.Status == DeliveryFailed || orig.Status == DeliveryUncertain {
		s.repo.Resolve(orig.ID)
	}
	s.bus.Publish(events.Event{Type: events.DeliveryQueued, PrinterID: reprint.PrinterID,
		DeliveryID: reprint.ExternalDeliveryID, Message: "reprint of " + orig.ExternalDeliveryID})
	if s.waker != nil {
		s.waker.Wake(reprint.PrinterID)
	}
	return reprint, nil
}

// Cancel cancels a queued/failed/uncertain delivery by external ID.
func (s *Service) Cancel(externalDeliveryID string) error {
	d, err := s.repo.GetDeliveryByExternalID(externalDeliveryID)
	if err != nil {
		return err
	}
	if err := s.repo.Cancel(d.ID); err != nil {
		return &ValidationError{msg: err.Error()}
	}
	s.bus.Publish(events.Event{Type: events.DeliveryCancelled, PrinterID: d.PrinterID, DeliveryID: d.ExternalDeliveryID})
	return nil
}

// Resolve acknowledges a failed/uncertain delivery without reprinting.
func (s *Service) Resolve(externalDeliveryID string) error {
	d, err := s.repo.GetDeliveryByExternalID(externalDeliveryID)
	if err != nil {
		return err
	}
	if err := s.repo.Resolve(d.ID); err != nil {
		return &ValidationError{msg: err.Error()}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
