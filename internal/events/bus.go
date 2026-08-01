// Package events is a small in-process pub/sub bus. Printer workers and the
// job service publish; the app wires subscribers that persist events to
// SQLite, log them, and (in a later phase) fan out to SSE clients.
package events

import (
	"sync"
	"time"
)

// Event types, used both on the bus and in the print_events table.
const (
	PrinterConnected    = "printer.connected"
	PrinterDisconnected = "printer.disconnected"
	PrinterReconnecting = "printer.reconnecting"
	PrinterError        = "printer.error"
	JobAccepted         = "job.accepted"
	DeliveryQueued      = "delivery.queued"
	DeliveryProcessing  = "delivery.processing"
	DeliveryTransmitted = "delivery.transmitted"
	DeliveryFailed      = "delivery.failed"
	DeliveryUncertain   = "delivery.uncertain"
	DeliveryCancelled   = "delivery.cancelled"
	ConfigChanged       = "config.changed"
	AuthDenied          = "auth.denied"
	AgentStarted        = "agent.started"
	AgentStopping       = "agent.stopping"
)

type Event struct {
	Type       string    `json:"type"`
	PrinterID  string    `json:"printerId,omitempty"`
	DeliveryID string    `json:"deliveryId,omitempty"`
	Message    string    `json:"message,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Bus struct {
	mu   sync.RWMutex
	subs []func(Event)
}

func NewBus() *Bus { return &Bus{} }

// Subscribe registers a handler invoked synchronously for every event.
// Handlers must be fast and must not publish.
func (b *Bus) Subscribe(fn func(Event)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, fn)
}

func (b *Bus) Publish(e Event) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	b.mu.RLock()
	subs := b.subs
	b.mu.RUnlock()
	for _, fn := range subs {
		fn(e)
	}
}
