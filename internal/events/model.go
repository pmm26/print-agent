// Package events defines the durable, sanitized lifecycle event model.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const SchemaVersion = "1.0"

type Scope string

const (
	ScopeExternal Scope = "external"
	ScopeLocal    Scope = "local"
)

type Durability string

const (
	DurabilityDomain      Durability = "domain"
	DurabilityOperational Durability = "operational"
)

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

type Event struct {
	SchemaVersion    string         `json:"schemaVersion"`
	EventID          string         `json:"eventId"`
	Sequence         int64          `json:"sequence"`
	Type             string         `json:"type"`
	OccurredAt       time.Time      `json:"occurredAt"`
	AgentID          string         `json:"agentId"`
	PublishScope     Scope          `json:"publishScope,omitempty"`
	PrinterID        string         `json:"printerId,omitempty"`
	JobUID           string         `json:"jobUid,omitempty"`
	JobID            string         `json:"jobId,omitempty"`
	Template         string         `json:"template,omitempty"`
	RunUID           string         `json:"printRunUid,omitempty"`
	RunNumber        int            `json:"runNumber,omitempty"`
	AttemptNumber    int            `json:"attemptNumber,omitempty"`
	Trigger          string         `json:"trigger,omitempty"`
	PreviousRunUID   string         `json:"previousPrintRunUid,omitempty"`
	ReprintOfRunUID  string         `json:"reprintOfPrintRunUid,omitempty"`
	CorrelationID    string         `json:"correlationId,omitempty"`
	CausationEventID string         `json:"causationEventId,omitempty"`
	Error            *Error         `json:"error,omitempty"`
	Message          string         `json:"safeMessage,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`

	Durability Durability `json:"-"`
	Category   string     `json:"-"`
	ExpiresAt  time.Time  `json:"-"`
}

func (e *Event) Prepare(agentID string, now time.Time, retention time.Duration) error {
	if e.Type == "" {
		return fmt.Errorf("event type is required")
	}
	if e.EventID == "" {
		id, err := newID()
		if err != nil {
			return err
		}
		e.EventID = id
	}
	if e.SchemaVersion == "" {
		e.SchemaVersion = SchemaVersion
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = now.UTC()
	} else {
		e.OccurredAt = e.OccurredAt.UTC()
	}
	if e.AgentID == "" {
		e.AgentID = agentID
	}
	if e.PublishScope == "" {
		e.PublishScope = ScopeExternal
	}
	if e.Durability == "" {
		e.Durability = DurabilityDomain
	}
	if e.Category == "" {
		e.Category = categoryFor(e.Type)
	}
	if e.ExpiresAt.IsZero() {
		e.ExpiresAt = now.UTC().Add(retention)
	}
	return ValidateSafe(*e)
}

func categoryFor(eventType string) string {
	prefix, _, _ := strings.Cut(eventType, ".")
	switch prefix {
	case "agent", "printer", "job", "websocket":
		return prefix
	case "print_run", "manual_reprint":
		return "print_run"
	case "auth":
		return "security"
	default:
		return "config"
	}
}

func CategoryFor(eventType string) string { return categoryFor(eventType) }

func ValidateSafe(e Event) error {
	encoded, err := json.Marshal(e)
	if err != nil {
		return err
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"authorization", "pairingpin", "pairing_pin", "receiptpayload", "escposbytes", "privatekey"} {
		if strings.Contains(lower, forbidden) {
			return fmt.Errorf("event contains forbidden sensitive field %q", forbidden)
		}
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate event id: %w", err)
	}
	return "evt_" + hex.EncodeToString(b), nil
}

type Publisher interface {
	Publish(Event) bool
}

type DiscardPublisher struct{}

func NewDiscardPublisher() *DiscardPublisher { return &DiscardPublisher{} }
func (*DiscardPublisher) Publish(Event) bool { return true }
