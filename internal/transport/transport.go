// Package transport abstracts the byte channel to a printer. The print
// engine depends only on the Transport interface, never on serial ports or
// RFCOMM directly.
package transport

import (
	"context"
	"errors"
)

// Sentinel errors used by the worker to classify failures.
var (
	// ErrUnsupported is returned by Probe when the printer offers no safe
	// status check.
	ErrUnsupported = errors.New("operation not supported by this transport")
	// ErrWriteTimeout is returned when a write did not complete in time.
	// The transport closes itself before returning it.
	ErrWriteTimeout = errors.New("write timed out")
)

// WriteError reports how much of the payload was handed to the OS before a
// write failed, which decides between the `failed` and `uncertain` delivery
// states.
type WriteError struct {
	BytesWritten int
	Err          error
}

func (e *WriteError) Error() string { return e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

// Transport is a connection to one printer.
type Transport interface {
	// Connect opens the endpoint. It must be safe to call again after Close.
	Connect(ctx context.Context) error
	// Write sends the full payload or returns a *WriteError. It must never
	// block past its internal deadline: a hung write closes the endpoint.
	Write(ctx context.Context, data []byte) error
	// Probe performs a safe liveness check, or returns ErrUnsupported.
	Probe(ctx context.Context) error
	Close() error
	Endpoint() string
}
