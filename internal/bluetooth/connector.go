// Package bluetooth resolves OS-level Bluetooth pairings into serial
// endpoints the transport layer can open. Pairing itself is delegated to the
// operating system's Bluetooth settings in v1.
package bluetooth

import (
	"context"
	"errors"

	"print-agent/internal/config"
)

// ErrNotConnected is returned by VerifyConnected when the OS positively
// reports the paired device as disconnected. On macOS, serial opens and
// writes to /dev/cu.* succeed (buffered by the OS) even with the printer
// off, so the write path alone cannot detect a dead link.
var ErrNotConnected = errors.New("device is paired but not connected")

// Candidate is a serial endpoint that may correspond to a paired printer.
type Candidate struct {
	Endpoint string `json:"endpoint"`
	// DeviceName is the paired Bluetooth device name when it could be
	// correlated with the endpoint, otherwise empty.
	DeviceName string `json:"deviceName,omitempty"`
	// DeviceAddress is the Bluetooth MAC when known.
	DeviceAddress string `json:"deviceAddress,omitempty"`
	// Connected reports the OS-level connection state when known.
	Connected bool `json:"connected"`
	// IsPrinter is true when the OS classifies the paired device as a
	// printer, making it the obvious pick in the add-printer wizard.
	IsPrinter bool `json:"isPrinter"`
}

// Connector ensures the platform-level Bluetooth serial endpoint exists.
type Connector interface {
	// EnsureConnected resolves (and if needed re-establishes) the serial
	// endpoint for the configured printer and returns it.
	EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (endpoint string, err error)
	// Disconnect releases any platform-level link. Optional; may be a no-op.
	Disconnect(ctx context.Context, cfg config.PrinterConfig) error
	// ListCandidates enumerates serial endpoints that could be printers.
	ListCandidates(ctx context.Context) ([]Candidate, error)
	// VerifyConnected returns nil when the device is connected or its state
	// cannot be determined, and ErrNotConnected when the OS positively
	// reports it disconnected. Implementations must be cheap enough to call
	// every few seconds (cache OS queries).
	VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error
	// OpenSystemBluetoothSettings opens the OS pairing UI.
	OpenSystemBluetoothSettings(ctx context.Context) error
}
