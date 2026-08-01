//go:build !darwin

package bluetooth

import (
	"context"
	"errors"
	"runtime"

	"print-agent/internal/config"
)

// NewPlatformConnector returns a stub on platforms whose adapter is not yet
// implemented (Windows: Phase 4, Linux: Phase 5 of the spec).
func NewPlatformConnector() Connector { return &stubConnector{} }

type stubConnector struct{}

var errUnimplemented = errors.New("bluetooth connector not implemented on " + runtime.GOOS)

func (s *stubConnector) EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (string, error) {
	// Trust the configured endpoint; the serial transport will surface a
	// clear error if it cannot be opened.
	if cfg.Endpoint != "" {
		return cfg.Endpoint, nil
	}
	return "", errUnimplemented
}

func (s *stubConnector) Disconnect(ctx context.Context, cfg config.PrinterConfig) error { return nil }

func (s *stubConnector) ListCandidates(ctx context.Context) ([]Candidate, error) {
	return nil, errUnimplemented
}

func (s *stubConnector) OpenSystemBluetoothSettings(ctx context.Context) error {
	return errUnimplemented
}
