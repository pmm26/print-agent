// Package jobs owns print jobs and their per-printer deliveries: acceptance
// with idempotency, persistence, state transitions, and reprints.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// DeliveryStatus is the lifecycle state of one document on one printer.
type DeliveryStatus string

const (
	DeliveryQueued      DeliveryStatus = "queued"
	DeliveryProcessing  DeliveryStatus = "processing"
	DeliveryTransmitted DeliveryStatus = "transmitted"
	DeliveryFailed      DeliveryStatus = "failed"
	DeliveryUncertain   DeliveryStatus = "uncertain"
	DeliveryCancelled   DeliveryStatus = "cancelled"
)

// Terminal reports whether no further automatic processing will happen.
func (s DeliveryStatus) Terminal() bool {
	switch s {
	case DeliveryTransmitted, DeliveryFailed, DeliveryUncertain, DeliveryCancelled:
		return true
	}
	return false
}

// Job is the master record for one POS request.
type Job struct {
	ID            string    `json:"-"`
	ExternalJobID string    `json:"jobId"`
	Source        string    `json:"source"`
	CreatedAt     time.Time `json:"createdAt"`
	AcceptedAt    time.Time `json:"acceptedAt"`
}

// Delivery is one document destined for one printer.
type Delivery struct {
	ID                 string          `json:"-"`
	ExternalDeliveryID string          `json:"deliveryId"`
	JobID              string          `json:"-"`
	PrinterID          string          `json:"printerId"`
	Template           string          `json:"template"`
	PayloadJSON        json.RawMessage `json:"-"`
	Status             DeliveryStatus  `json:"status"`
	AttemptCount       int             `json:"attemptCount"`
	BytesWritten       int             `json:"bytesWritten"`
	LastError          string          `json:"lastError,omitempty"`
	ReprintOf          string          `json:"reprintOf,omitempty"`
	CreatedAt          time.Time       `json:"createdAt"`
	StartedAt          *time.Time      `json:"startedAt,omitempty"`
	TransmittedAt      *time.Time      `json:"transmittedAt,omitempty"`
	ResolvedAt         *time.Time      `json:"resolvedAt,omitempty"`
}

// JobStatus aggregates delivery states into the job-level status exposed by
// the API (the spec defines only delivery states).
func JobStatus(deliveries []Delivery) string {
	if len(deliveries) == 0 {
		return "empty"
	}
	anyPending, anyProcessing, anyAttention := false, false, false
	for _, d := range deliveries {
		switch d.Status {
		case DeliveryQueued:
			anyPending = true
		case DeliveryProcessing:
			anyProcessing = true
		case DeliveryFailed, DeliveryUncertain:
			if d.ResolvedAt == nil {
				anyAttention = true
			}
		}
	}
	switch {
	case anyProcessing:
		return "processing"
	case anyPending && anyAttention:
		return "attention"
	case anyPending:
		return "queued"
	case anyAttention:
		return "attention"
	default:
		return "completed"
	}
}

// newID returns a random 128-bit hex identifier for internal keys.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
