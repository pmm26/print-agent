// Package platform isolates everything OS-specific behind the Driver
// interface. The core of the agent (jobs, workers, API, rendering, storage)
// never mentions an operating system; adding platform support means filling
// in one platform subfolder and nothing else.
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
// reports the paired device as disconnected. Some Bluetooth serial stacks
// can accept buffered writes while the printer is off, so the write path
// alone cannot detect a dead link.
var ErrNotConnected = errors.New("device is paired but not connected")

// Bluetooth management errors let the local API return useful, stable error
// codes without exposing OS-specific implementation details.
var (
	ErrInvalidBluetoothAddress = errors.New("invalid Bluetooth address")
	ErrBluetoothDeviceNotFound = errors.New("Bluetooth device not found")
	ErrBluetoothUnavailable    = errors.New("Bluetooth is unavailable")
	ErrBluetoothPairInProgress = errors.New("Bluetooth pairing is already in progress")
	ErrBluetoothPairRejected   = errors.New("Bluetooth pairing was rejected")
	ErrBluetoothPairTimeout    = errors.New("Bluetooth pairing timed out")
	ErrBluetoothPairFailed     = errors.New("Bluetooth pairing failed")
)

type LinkState string

const (
	LinkUnknown      LinkState = "unknown"
	LinkConnected    LinkState = "connected"
	LinkDisconnected LinkState = "disconnected"
)

// LinkVerifier is implemented by production drivers that can distinguish a
// confirmed link from an unknown state. Workers require confirmation before
// claiming Bluetooth work.
type LinkVerifier interface {
	LinkState(ctx context.Context, cfg config.PrinterConfig) (LinkState, error)
}

// Candidate is a platform endpoint that may correspond to a Bluetooth printer.
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

// BluetoothDevice is a device visible to the host Bluetooth stack. Unlike a
// Candidate it may not be paired yet and therefore may not have a usable
// printer endpoint.
type BluetoothDevice struct {
	Name      string `json:"name,omitempty"`
	Address   string `json:"address"`
	Paired    bool   `json:"paired"`
	Connected bool   `json:"connected"`
	IsPrinter bool   `json:"isPrinter"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// BluetoothPairer is an optional capability implemented by platforms which
// can perform discovery and pairing without handing off to a desktop UI.
// Callers must feature-detect this capability.
type BluetoothPairer interface {
	ListBluetoothDevices(ctx context.Context) ([]BluetoothDevice, error)
	StartBluetoothDiscovery(ctx context.Context) error
	StopBluetoothDiscovery(ctx context.Context) error
	PairBluetoothDevice(ctx context.Context, address, pin string) (BluetoothDevice, error)
}

// Driver is one platform's implementation of Bluetooth-printer plumbing.
type Driver interface {
	// Name identifies the driver for diagnostics.
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
	// substitute its own.
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
