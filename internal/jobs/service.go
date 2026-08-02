package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
)

const (
	maxPayloadBytes     = 64 * 1024
	maxPrinters         = 20
	MaxAutomaticRetries = 3
)

type Waker interface{ Wake(printerID string) }
type PrinterDirectory interface {
	PrinterExists(id string) bool
	PrinterConfig(id string) (config.PrinterConfig, error)
}
type AcceptanceGuard interface{ BeginAcceptance() func() }
type TemplateValidator func(name string) bool
type PayloadValidator func(name string, data json.RawMessage) error

type Service struct {
	repo      *Repository
	publisher events.Publisher
	waker     Waker
	printers  PrinterDirectory
	renderer  *escpos.Renderer
	templates TemplateValidator
	payloads  PayloadValidator
}

func NewService(repo *Repository, publisher events.Publisher, printers PrinterDirectory,
	renderer *escpos.Renderer, templates TemplateValidator) *Service {
	if renderer == nil {
		renderer = escpos.NewRenderer()
	}
	return &Service{repo: repo, publisher: publisher, printers: printers, renderer: renderer, templates: templates}
}

func (s *Service) SetWaker(w Waker)                       { s.waker = w }
func (s *Service) SetPayloadValidator(v PayloadValidator) { s.payloads = v }

type CreateJobRequest struct {
	JobID      string          `json:"jobId"`
	Template   string          `json:"template"`
	Data       json.RawMessage `json:"data"`
	PrinterIDs []string        `json:"printerIds"`
	Source     string          `json:"-"`
	Owner      string          `json:"-"`
}

type ReprintRequest struct {
	RequestID  string   `json:"reprintRequestId"`
	PrinterIDs []string `json:"printerIds"`
	Reason     string   `json:"reason,omitempty"`
}

type ReprintResult struct {
	JobUID    string     `json:"jobUid"`
	RequestID string     `json:"reprintRequestId"`
	Duplicate bool       `json:"duplicate,omitempty"`
	PrintRuns []PrintRun `json:"printRuns"`
}

type ValidationError struct{ msg string }

func (e *ValidationError) Error() string             { return e.msg }
func NewValidationError(msg string) *ValidationError { return &ValidationError{msg: msg} }
func validationErrorf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

type ConflictError struct {
	Code string
	msg  string
}

func (e *ConflictError) Error() string           { return e.msg }
func NewConflictError(msg string) *ConflictError { return &ConflictError{Code: "conflict", msg: msg} }
func conflict(code, format string, args ...any) *ConflictError {
	return &ConflictError{Code: code, msg: fmt.Sprintf(format, args...)}
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON value")
	}
	return json.Marshal(value)
}

func canonicalPrinterIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > maxPrinters {
		return nil, validationErrorf("printerIds must contain between 1 and %d entries", maxPrinters)
	}
	out := append([]string(nil), ids...)
	sort.Strings(out)
	for i, id := range out {
		if id == "" {
			return nil, validationErrorf("printerIds[%d] is empty", i)
		}
		if i > 0 && id == out[i-1] {
			return nil, validationErrorf("duplicate printerId %q", id)
		}
	}
	return out, nil
}

func jobRequestHash(jobID, template string, data json.RawMessage, printerIDs []string) (string, error) {
	payload, err := json.Marshal(struct {
		Version    int             `json:"version"`
		JobID      string          `json:"jobId"`
		Template   string          `json:"template"`
		Data       json.RawMessage `json:"data"`
		PrinterIDs []string        `json:"printerIds"`
	}{1, jobID, template, data, printerIDs})
	if err != nil {
		return "", err
	}
	return hashBytes(payload), nil
}

func reprintRequestHash(req ReprintRequest, printerIDs []string) (string, error) {
	payload, err := json.Marshal(struct {
		Version    int      `json:"version"`
		PrinterIDs []string `json:"printerIds"`
		Reason     string   `json:"reason"`
	}{1, printerIDs, req.Reason})
	if err != nil {
		return "", err
	}
	return hashBytes(payload), nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ContentHash returns the persisted SHA-256 representation of rendered bytes.
func ContentHash(data []byte) string { return hashBytes(data) }

func renderConfig(target JobPrinter) config.PrinterConfig {
	return config.PrinterConfig{ID: target.PrinterID, Encoding: target.Encoding,
		CharactersPerLine: target.CharactersPerLine}
}

func renderHash(renderer *escpos.Renderer, job Job, target JobPrinter, mode ContentMode, runNumber int) (string, error) {
	normal, err := renderer.Render(job.Template, job.Data, renderConfig(target), escpos.RenderOptions{AcceptedAt: job.CreatedAt})
	if err != nil {
		return "", err
	}
	normalHash := hashBytes(normal)
	if target.AcceptedContentHash != "" && normalHash != target.AcceptedContentHash {
		return "", fmt.Errorf("accepted template output changed for printer %s", target.PrinterID)
	}
	if mode == ContentNormal {
		return normalHash, nil
	}
	reprint, err := renderer.Render(job.Template, job.Data, renderConfig(target), escpos.RenderOptions{
		Reprint: true, RunNumber: runNumber, AcceptedAt: job.CreatedAt,
	})
	if err != nil {
		return "", err
	}
	return hashBytes(reprint), nil
}

// ExpectedContentHash is shared with workers when materializing and verifying Runs.
func ExpectedContentHash(renderer *escpos.Renderer, job Job, target JobPrinter, mode ContentMode, runNumber int) (string, error) {
	return renderHash(renderer, job, target, mode, runNumber)
}

func (s *Service) Accept(req CreateJobRequest) (result JobDetail, resultErr error) {
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.JobReceived, PublishScope: events.ScopeLocal,
			JobID: req.JobID, Template: req.Template, Message: "best-effort request receipt"})
		defer func() {
			if resultErr != nil {
				s.publisher.Publish(events.Event{Type: events.JobRejected, JobID: req.JobID,
					Template: req.Template, Error: &events.Error{Code: "job_rejected", Message: "job request was not accepted"}})
			}
		}()
	}
	if guard, ok := s.printers.(AcceptanceGuard); ok {
		release := guard.BeginAcceptance()
		defer release()
	}
	if req.Owner == "" {
		req.Owner = "local"
	}
	if req.Source == "" {
		req.Source = req.Owner
	}
	if req.JobID == "" || len(req.JobID) > 200 {
		return JobDetail{}, validationErrorf("jobId is required and must be at most 200 bytes")
	}
	if req.Template == "" || len(req.Template) > 100 {
		return JobDetail{}, validationErrorf("template is required and must be at most 100 bytes")
	}
	if len(req.Data) == 0 || len(req.Data) > maxPayloadBytes {
		return JobDetail{}, validationErrorf("data is required and must not exceed %d bytes", maxPayloadBytes)
	}
	data, err := canonicalJSON(req.Data)
	if err != nil {
		return JobDetail{}, validationErrorf("data is invalid JSON: %v", err)
	}
	canonicalIDs, err := canonicalPrinterIDs(req.PrinterIDs)
	if err != nil {
		return JobDetail{}, err
	}
	hash, err := jobRequestHash(req.JobID, req.Template, data, canonicalIDs)
	if err != nil {
		return JobDetail{}, err
	}
	if existing, err := s.repo.GetJobByIdentity(req.JobID, req.Template); err == nil {
		return s.duplicateResult(existing, req.Owner, hash)
	} else if !errors.Is(err, ErrNotFound) {
		return JobDetail{}, err
	}
	if !s.templates(req.Template) {
		return JobDetail{}, validationErrorf("unknown template %q", req.Template)
	}
	if s.payloads != nil {
		if err := s.payloads(req.Template, data); err != nil {
			return JobDetail{}, validationErrorf("data: %v", err)
		}
	}
	for _, printerID := range canonicalIDs {
		if !s.printers.PrinterExists(printerID) {
			return JobDetail{}, validationErrorf("unknown or retired printerId %q", printerID)
		}
	}

	now := time.Now().UTC()
	uid, err := newID("job_")
	if err != nil {
		return JobDetail{}, err
	}
	job := Job{UID: uid, JobID: req.JobID, Template: req.Template, Data: data,
		RequestHash: hash, Source: req.Source, OwnerOrigin: req.Owner,
		CreatedAt: now, ExpiresAt: now.Add(retentionWindow)}
	targets := make([]JobPrinter, 0, len(req.PrinterIDs))
	runs := make([]PrintRun, 0, len(req.PrinterIDs))
	for order, printerID := range req.PrinterIDs {
		cfg, err := s.printers.PrinterConfig(printerID)
		if err != nil {
			return JobDetail{}, validationErrorf("printerId %q is no longer available", printerID)
		}
		target := JobPrinter{JobUID: uid, PrinterID: printerID, TargetOrder: order,
			Encoding: cfg.Encoding, CharactersPerLine: cfg.CharactersPerLine}
		content, err := s.renderer.Render(req.Template, data, cfg, escpos.RenderOptions{AcceptedAt: now})
		if err != nil {
			return JobDetail{}, validationErrorf("render for printer %q: %v", printerID, err)
		}
		target.AcceptedContentHash = hashBytes(content)
		runUID, err := newID("run_")
		if err != nil {
			return JobDetail{}, err
		}
		targets = append(targets, target)
		runs = append(runs, PrintRun{UID: runUID, JobUID: uid, PrinterID: printerID,
			ChainUID: runUID, RunNumber: 1, AttemptNumber: 1, Trigger: TriggerInitial,
			ContentMode: ContentNormal, ExpectedContentHash: target.AcceptedContentHash,
			Status: RunQueued, RetryDisposition: RetryNone, CreatedAt: now})
	}
	if err := s.repo.InsertJob(job, targets, runs); err != nil {
		if isUniqueViolation(err) {
			existing, lookupErr := s.repo.GetJobByIdentity(req.JobID, req.Template)
			if lookupErr == nil {
				return s.duplicateResult(existing, req.Owner, hash)
			}
		}
		return JobDetail{}, err
	}
	detail := DeriveJob(job, targets, runs)
	for _, run := range runs {
		s.publishQueued(run)
	}
	return detail, nil
}

func (s *Service) duplicateResult(existing Job, owner, hash string) (JobDetail, error) {
	if owner != "local" && existing.OwnerOrigin != owner {
		return JobDetail{}, conflict("idempotency_conflict", "jobId and template are already in use")
	}
	if existing.RequestHash != hash {
		return JobDetail{}, conflict("idempotency_conflict", "jobId and template were already accepted with different data or printers")
	}
	detail, err := s.repo.GetDetail(existing.UID)
	if err != nil {
		return JobDetail{}, err
	}
	detail.Duplicate = true
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.JobDuplicateRecognized, JobUID: existing.UID,
			JobID: existing.JobID, Template: existing.Template, CorrelationID: existing.UID})
	}
	return detail, nil
}

func (s *Service) publishQueued(run PrintRun) {
	if s.waker != nil {
		s.waker.Wake(run.PrinterID)
	}
}

func (s *Service) Get(uid, owner string) (JobDetail, error) {
	detail, err := s.repo.GetDetail(uid)
	if err != nil {
		return JobDetail{}, err
	}
	if owner != "" && owner != "local" && detail.OwnerOrigin != owner {
		return JobDetail{}, ErrNotFound
	}
	return detail, nil
}

func (s *Service) List(jobID, template, attention, owner string, limit int, before *time.Time, beforeUID string) ([]JobListItem, error) {
	details, err := s.repo.ListJobDetails(jobID, template, attention, owner, limit, before, beforeUID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	out := make([]JobListItem, 0, min(len(details), limit))
	for i := range details {
		out = append(out, details[i].ListItem())
	}
	return out, nil
}

func (s *Service) GetRun(uid, owner string) (PrintRun, error) {
	run, err := s.repo.GetRun(uid)
	if err != nil {
		return PrintRun{}, err
	}
	job, err := s.repo.GetJob(run.JobUID)
	if err != nil {
		return PrintRun{}, err
	}
	if owner != "" && owner != "local" && job.OwnerOrigin != owner {
		return PrintRun{}, ErrNotFound
	}
	return run, nil
}

func (s *Service) Reprint(jobUID string, req ReprintRequest, owner string) (ReprintResult, error) {
	if guard, ok := s.printers.(AcceptanceGuard); ok {
		release := guard.BeginAcceptance()
		defer release()
	}
	if req.RequestID == "" || len(req.RequestID) > 250 {
		return ReprintResult{}, validationErrorf("reprintRequestId is required and must be at most 250 bytes")
	}
	if len(req.Reason) > 500 {
		return ReprintResult{}, validationErrorf("reason must be at most 500 bytes")
	}
	ids, err := canonicalPrinterIDs(req.PrinterIDs)
	if err != nil {
		return ReprintResult{}, err
	}
	job, err := s.repo.GetJob(jobUID)
	if err != nil {
		return ReprintResult{}, err
	}
	if owner != "" && owner != "local" && job.OwnerOrigin != owner {
		return ReprintResult{}, ErrNotFound
	}
	hash, err := reprintRequestHash(req, ids)
	if err != nil {
		return ReprintResult{}, err
	}
	if rec, err := s.repo.GetDeduplication("manual_reprint", jobUID, req.RequestID); err == nil {
		return s.reprintDuplicate(jobUID, req.RequestID, hash, rec)
	} else if !errors.Is(err, ErrNotFound) {
		return ReprintResult{}, err
	}
	for _, printerID := range ids {
		if !s.printers.PrinterExists(printerID) {
			return ReprintResult{}, conflict("printer_unavailable", "original printer %q is retired or unavailable", printerID)
		}
	}
	runs, err := s.repo.InsertManualReprints(jobUID, req.RequestID, hash, req.Reason, ids,
		func(job Job, target JobPrinter, runNumber int) (string, error) {
			contentHash, err := renderHash(s.renderer, job, target, ContentReprint, runNumber)
			if err != nil {
				return "", conflict("content_changed", "accepted Job content can no longer be rendered safely: %v", err)
			}
			return contentHash, nil
		})
	if errors.Is(err, ErrDuplicateRequest) {
		rec, lookupErr := s.repo.GetDeduplication("manual_reprint", jobUID, req.RequestID)
		if lookupErr != nil {
			return ReprintResult{}, lookupErr
		}
		return s.reprintDuplicate(jobUID, req.RequestID, hash, rec)
	}
	if errors.Is(err, ErrStateChanged) || isUniqueViolation(err) {
		return ReprintResult{}, conflict("active_print_run", "a Print Run is already queued, claimed, or transmitting for one of the selected printers")
	}
	if err != nil {
		return ReprintResult{}, err
	}
	for _, run := range runs {
		s.publishQueued(run)
	}
	return ReprintResult{JobUID: jobUID, RequestID: req.RequestID, PrintRuns: runs}, nil
}

func (s *Service) reprintDuplicate(jobUID, requestID, hash string, rec DeduplicationRecord) (ReprintResult, error) {
	if rec.Hash != hash {
		return ReprintResult{}, conflict("idempotency_conflict", "reprintRequestId was already used with different printers or reason")
	}
	runs, err := s.repo.RunsForReprintRequest(jobUID, requestID)
	if err != nil {
		return ReprintResult{}, err
	}
	return ReprintResult{JobUID: jobUID, RequestID: requestID, Duplicate: true, PrintRuns: runs}, nil
}

func (s *Service) ConfirmPrinted(runUID string) error {
	if err := s.repo.ResolveUncertain(runUID, ResolutionConfirmedPrinted); err != nil {
		return conflict("invalid_state", "only an unresolved uncertain Print Run can be confirmed")
	}
	return nil
}

func (s *Service) CancelRun(runUID string) error {
	if err := s.repo.CancelRun(runUID); err != nil {
		return conflict("invalid_state", "only a queued Print Run can be cancelled")
	}
	return nil
}

func (s *Service) CancelJobTargets(jobUID string, printerIDs []string, reason string) error {
	ids, err := canonicalPrinterIDs(printerIDs)
	if err != nil {
		return err
	}
	if len(reason) > 500 {
		return validationErrorf("reason must be at most 500 bytes")
	}
	if err := s.repo.CancelTargets(jobUID, ids, reason); err != nil {
		if errors.Is(err, ErrTargetFulfilled) {
			return conflict("invalid_state", "an already fulfilled original printer target cannot be cancelled")
		}
		if errors.Is(err, ErrStateChanged) {
			return conflict("active_print_run", "a selected printer has a claimed or transmitting Run")
		}
		return err
	}
	return nil
}

func (s *Service) MaxAutoRetries() int { return MaxAutomaticRetries }
