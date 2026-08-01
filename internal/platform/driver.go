// Package platform isolates everything OS-specific behind the Driver
// interface. The core of the agent (jobs, workers, API, rendering, storage)
// never mentions an operating system; adding platform support means filling
// in one subfolder (darwin/, linux/, windows/) and nothing else.
package platform

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"print-agent/internal/config"
	"print-agent/internal/transport"
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

// Driver is one platform's implementation of Bluetooth-printer plumbing.
// Pairing itself is delegated to the OS Bluetooth settings in v1.
type Driver interface {
	// Name identifies the driver ("darwin", "linux", …) for diagnostics.
	Name() string
	// EnsureConnected resolves (and if needed re-establishes) the endpoint
	// for the configured printer and returns it.
	EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (endpoint string, err error)
	// Disconnect releases any platform-level link. Optional; may be a no-op.
	Disconnect(ctx context.Context, cfg config.PrinterConfig) error
	// ListCandidates enumerates endpoints that could be printers.
	ListCandidates(ctx context.Context) ([]Candidate, error)
	// VerifyConnected returns nil when the device is connected or its state
	// cannot be determined, and ErrNotConnected when the OS positively
	// reports it disconnected. Implementations must be cheap enough to call
	// every few seconds (cache OS queries).
	VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error
	// OpenSystemBluetoothSettings opens the OS pairing UI.
	OpenSystemBluetoothSettings(ctx context.Context) error
	// NewTransport returns the byte channel used to talk to this printer.
	// Most platforms use the shared serial transport; a platform may
	// substitute its own (e.g. Linux RFCOMM sockets).
	NewTransport(cfg config.PrinterConfig) transport.Transport
}

// UnimplementedDriver is an embeddable base with safe defaults, in the
// style of gRPC's Unimplemented*Server: stubs and test fakes embed it and
// override only what they support.
type UnimplementedDriver struct{}

func (UnimplementedDriver) Name() string { return "unimplemented" }

// EnsureConnected trusts the configured endpoint; the transport surfaces a
// clear error if it cannot be opened.
func (UnimplementedDriver) EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (string, error) {
	if cfg.Endpoint != "" {
		return cfg.Endpoint, nil
	}
	return "", fmt.Errorf("no endpoint configured and endpoint discovery is not implemented on %s", runtime.GOOS)
}

func (UnimplementedDriver) Disconnect(ctx context.Context, cfg config.PrinterConfig) error {
	return nil
}

func (UnimplementedDriver) ListCandidates(ctx context.Context) ([]Candidate, error) {
	return nil, fmt.Errorf("endpoint discovery is not implemented on %s", runtime.GOOS)
}

// VerifyConnected reports "unknown" (nil): without an OS-level view, the
// write path is the only failure detector.
func (UnimplementedDriver) VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error {
	return nil
}

func (UnimplementedDriver) OpenSystemBluetoothSettings(ctx context.Context) error {
	return fmt.Errorf("opening Bluetooth settings is not implemented on %s", runtime.GOOS)
}

// NewTransport defaults to the shared cross-platform serial transport.
func (UnimplementedDriver) NewTransport(cfg config.PrinterConfig) transport.Transport {
	return transport.NewSerial(cfg)
}
