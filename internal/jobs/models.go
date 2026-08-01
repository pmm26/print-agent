// Package jobs owns immutable Jobs and their per-printer Print Runs.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type PrintRunStatus string

const (
	RunQueued      PrintRunStatus = "queued"
	RunProcessing  PrintRunStatus = "processing"
	RunTransmitted PrintRunStatus = "transmitted"
	RunFailed      PrintRunStatus = "failed"
	RunUncertain   PrintRunStatus = "uncertain"
	RunCancelled   PrintRunStatus = "cancelled"
)

func (s PrintRunStatus) Terminal() bool {
	return s == RunTransmitted || s == RunFailed || s == RunUncertain || s == RunCancelled
}

func ValidPrintRunStatus(s PrintRunStatus) bool {
	switch s {
	case RunQueued, RunProcessing, RunTransmitted, RunFailed, RunUncertain, RunCancelled:
		return true
	}
	return false
}

type PrintRunTrigger string

const (
	TriggerInitial        PrintRunTrigger = "initial"
	TriggerAutomaticRetry PrintRunTrigger = "automatic_retry"
	TriggerManualReprint  PrintRunTrigger = "manual_reprint"
)

type ContentMode string

const (
	ContentNormal  ContentMode = "normal"
	ContentReprint ContentMode = "reprint"
)

type RunResolution string

const (
	ResolutionConfirmedPrinted RunResolution = "confirmed_printed"
	ResolutionReprintRequested RunResolution = "reprint_requested"
)

type Job struct {
	UID         string          `json:"uid"`
	JobID       string          `json:"jobId"`
	Template    string          `json:"template"`
	Data        json.RawMessage `json:"data,omitempty"`
	RequestHash string          `json:"-"`
	Source      string          `json:"source,omitempty"`
	OwnerOrigin string          `json:"-"`
	CreatedAt   time.Time       `json:"createdAt"`
	ExpiresAt   time.Time       `json:"expiresAt"`
}

// JobPrinter is the persisted original-printer membership and render profile
// for a Job. It is supporting Job data, not a third domain concept.
type JobPrinter struct {
	JobUID              string     `json:"-"`
	PrinterID           string     `json:"printerId"`
	TargetOrder         int        `json:"-"`
	Encoding            string     `json:"-"`
	CharactersPerLine   int        `json:"-"`
	AcceptedContentHash string     `json:"-"`
	CancelledAt         *time.Time `json:"cancelledAt,omitempty"`
	CancellationReason  string     `json:"cancellationReason,omitempty"`
}

type PrintRun struct {
	QueueSequence       int64           `json:"-"`
	UID                 string          `json:"uid"`
	JobUID              string          `json:"jobUid"`
	PrinterID           string          `json:"printerId"`
	RunNumber           int             `json:"runNumber"`
	Trigger             PrintRunTrigger `json:"trigger"`
	ContentMode         ContentMode     `json:"-"`
	PreviousRunUID      string          `json:"previousRunUid,omitempty"`
	ReprintOfRunUID     string          `json:"reprintOfRunUid,omitempty"`
	ReprintRequestID    string          `json:"reprintRequestId,omitempty"`
	ReprintReason       string          `json:"reprintReason,omitempty"`
	ExpectedContentHash string          `json:"contentHash"`
	Status              PrintRunStatus  `json:"status"`
	Retryable           bool            `json:"retryPending,omitempty"`
	Resolution          RunResolution   `json:"resolution,omitempty"`
	ErrorCode           string          `json:"errorCode,omitempty"`
	ErrorMessage        string          `json:"errorMessage,omitempty"`
	BytesAccepted       int             `json:"bytesAccepted"`
	CreatedAt           time.Time       `json:"createdAt"`
	StartedAt           *time.Time      `json:"startedAt,omitempty"`
	FinishedAt          *time.Time      `json:"finishedAt,omitempty"`
	TransmittedAt       *time.Time      `json:"transmittedAt,omitempty"`
	ResolvedAt          *time.Time      `json:"resolvedAt,omitempty"`
}

type PrinterFulfillment struct {
	PrinterID    string     `json:"printerId"`
	Fulfilled    bool       `json:"fulfilled"`
	FulfilledAt  *time.Time `json:"fulfilledAt,omitempty"`
	Cancelled    bool       `json:"cancelled,omitempty"`
	RetryPending bool       `json:"retryPending,omitempty"`
	LatestRun    *PrintRun  `json:"latestRun,omitempty"`
	Runs         []PrintRun `json:"runs,omitempty"`
}

type JobDetail struct {
	Job
	UpdatedAt                      time.Time            `json:"updatedAt"`
	CompletedAt                    *time.Time           `json:"completedAt,omitempty"`
	State                          string               `json:"state"`
	FulfilledPrinterCount          int                  `json:"fulfilledPrinterCount"`
	OriginalPrinterCount           int                  `json:"originalPrinterCount"`
	PartiallyFulfilled             bool                 `json:"partiallyFulfilled"`
	RequiresAttention              bool                 `json:"requiresAttention"`
	HasUncertainResult             bool                 `json:"hasUncertainResult"`
	HasManualReprints              bool                 `json:"hasManualReprints"`
	ManualReprintRequiresAttention bool                 `json:"manualReprintRequiresAttention"`
	Duplicate                      bool                 `json:"duplicate,omitempty"`
	OriginalPrinters               []PrinterFulfillment `json:"originalPrinters"`
}

type JobListItem struct {
	UID                            string     `json:"uid"`
	JobID                          string     `json:"jobId"`
	Template                       string     `json:"template"`
	CreatedAt                      time.Time  `json:"createdAt"`
	UpdatedAt                      time.Time  `json:"updatedAt"`
	CompletedAt                    *time.Time `json:"completedAt,omitempty"`
	State                          string     `json:"state"`
	FulfilledPrinterCount          int        `json:"fulfilledPrinterCount"`
	OriginalPrinterCount           int        `json:"originalPrinterCount"`
	PartiallyFulfilled             bool       `json:"partiallyFulfilled"`
	RequiresAttention              bool       `json:"requiresAttention"`
	HasUncertainResult             bool       `json:"hasUncertainResult"`
	HasManualReprints              bool       `json:"hasManualReprints"`
	ManualReprintRequiresAttention bool       `json:"manualReprintRequiresAttention"`
}

func DeriveJob(job Job, printers []JobPrinter, runs []PrintRun) JobDetail {
	byPrinter := make(map[string][]PrintRun, len(printers))
	for _, run := range runs {
		byPrinter[run.PrinterID] = append(byPrinter[run.PrinterID], run)
	}

	detail := JobDetail{Job: job, UpdatedAt: job.CreatedAt, OriginalPrinterCount: len(printers)}
	var targetFulfilledTimes []time.Time
	anyUnfulfilledProcessing := false
	anyUnfulfilledQueued := false
	anyUnfulfilledAttention := false
	allUnfulfilledCancelled := true

	for _, target := range printers {
		printerRuns := byPrinter[target.PrinterID]
		p := PrinterFulfillment{PrinterID: target.PrinterID, Cancelled: target.CancelledAt != nil, Runs: printerRuns}
		if target.CancelledAt != nil && target.CancelledAt.After(detail.UpdatedAt) {
			detail.UpdatedAt = *target.CancelledAt
		}
		if len(printerRuns) > 0 {
			latest := printerRuns[len(printerRuns)-1]
			p.LatestRun = &latest
		}
		for i := range printerRuns {
			run := printerRuns[i]
			for _, stamp := range []*time.Time{&run.CreatedAt, run.StartedAt, run.FinishedAt, run.ResolvedAt} {
				if stamp != nil && stamp.After(detail.UpdatedAt) {
					detail.UpdatedAt = *stamp
				}
			}
			if run.Status == RunTransmitted || (run.Status == RunUncertain && run.Resolution == ResolutionConfirmedPrinted) {
				p.Fulfilled = true
				if p.FulfilledAt == nil {
					stamp := run.TransmittedAt
					if run.Status == RunUncertain {
						stamp = run.ResolvedAt
					}
					if stamp == nil {
						stamp = run.FinishedAt
					}
					p.FulfilledAt = stamp
				}
			}
			if run.Status == RunUncertain {
				detail.HasUncertainResult = true
			}
			if run.Trigger == TriggerManualReprint {
				detail.HasManualReprints = true
			}
			if run.Status == RunFailed && run.Retryable && run.Resolution == "" {
				p.RetryPending = true
			}
			if run.ContentMode == ContentReprint && (run.Status == RunFailed || run.Status == RunUncertain) && run.Resolution == "" && !run.Retryable {
				detail.ManualReprintRequiresAttention = true
			}
		}
		if p.Fulfilled {
			detail.FulfilledPrinterCount++
			if p.FulfilledAt != nil {
				targetFulfilledTimes = append(targetFulfilledTimes, *p.FulfilledAt)
			}
		} else {
			allUnfulfilledCancelled = allUnfulfilledCancelled && p.Cancelled
			for _, run := range printerRuns {
				switch run.Status {
				case RunProcessing:
					anyUnfulfilledProcessing = true
				case RunQueued:
					anyUnfulfilledQueued = true
				case RunFailed:
					if run.Retryable && run.Resolution == "" {
						anyUnfulfilledQueued = true
					} else if run.Resolution == "" && !p.Cancelled {
						anyUnfulfilledAttention = true
					}
				case RunUncertain:
					if run.Resolution == "" && !p.Cancelled {
						anyUnfulfilledAttention = true
					}
				}
			}
		}
		detail.OriginalPrinters = append(detail.OriginalPrinters, p)
	}

	detail.PartiallyFulfilled = detail.FulfilledPrinterCount > 0 && detail.FulfilledPrinterCount < detail.OriginalPrinterCount
	detail.RequiresAttention = anyUnfulfilledAttention || detail.ManualReprintRequiresAttention
	switch {
	case detail.OriginalPrinterCount > 0 && detail.FulfilledPrinterCount == detail.OriginalPrinterCount:
		detail.State = "completed"
		for i := range targetFulfilledTimes {
			if detail.CompletedAt == nil || targetFulfilledTimes[i].After(*detail.CompletedAt) {
				stamp := targetFulfilledTimes[i]
				detail.CompletedAt = &stamp
			}
		}
	case anyUnfulfilledProcessing:
		detail.State = "printing"
	case anyUnfulfilledQueued:
		detail.State = "queued"
	case anyUnfulfilledAttention:
		detail.State = "attention_required"
	case detail.FulfilledPrinterCount > 0 && allUnfulfilledCancelled:
		detail.State = "partially_completed"
	case detail.FulfilledPrinterCount == 0 && allUnfulfilledCancelled:
		detail.State = "cancelled"
	default:
		detail.State = "attention_required"
		detail.RequiresAttention = true
	}
	return detail
}

func (d JobDetail) ListItem() JobListItem {
	return JobListItem{
		UID: d.UID, JobID: d.JobID, Template: d.Template, CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt, CompletedAt: d.CompletedAt,
		State: d.State, FulfilledPrinterCount: d.FulfilledPrinterCount,
		OriginalPrinterCount: d.OriginalPrinterCount, PartiallyFulfilled: d.PartiallyFulfilled,
		RequiresAttention: d.RequiresAttention, HasUncertainResult: d.HasUncertainResult,
		HasManualReprints:              d.HasManualReprints,
		ManualReprintRequiresAttention: d.ManualReprintRequiresAttention,
	}
}

func newID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}
