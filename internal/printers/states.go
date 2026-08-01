// Package printers runs one independent worker per configured printer:
// connection state machine, automatic reconnect, and queue processing.
// A failure on one printer never blocks the others.
package printers

import (
	"time"

	"print-agent/internal/config"
)

// ConnectionState is the printer connection lifecycle state.
type ConnectionState string

const (
	StateDisabled     ConnectionState = "disabled"
	StateDisconnected ConnectionState = "disconnected"
	StateConnecting   ConnectionState = "connecting"
	StateConnected    ConnectionState = "connected"
	StatePrinting     ConnectionState = "printing"
	StateReconnecting ConnectionState = "reconnecting"
	StateError        ConnectionState = "error"
)

// Status is a point-in-time snapshot of one printer for the API/dashboard.
type Status struct {
	Printer          config.PrinterConfig `json:"printer"`
	State            ConnectionState      `json:"state"`
	Endpoint         string               `json:"endpoint"`
	LastError        string               `json:"lastError,omitempty"`
	LastTransmission *time.Time           `json:"lastTransmission,omitempty"`
	QueueDepth       int                  `json:"queueDepth"`
	AttentionCount   int                  `json:"attentionCount"`
	ReconnectAttempt int                  `json:"reconnectAttempt,omitempty"`
	NextRetryAt      *time.Time           `json:"nextRetryAt,omitempty"`
}

// reconnectDelay implements the spec's backoff schedule:
// 2s, 5s, 10s, then every 30s.
func reconnectDelay(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 2 * time.Second
	case attempt == 2:
		return 5 * time.Second
	case attempt == 3:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}
