//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/transport"
)

const (
	gattWriteTimeout = 10 * time.Second
	gattChunkDelay   = 10 * time.Millisecond
)

type gattConnection interface {
	WriteValue(context.Context, []byte) error
	MTU() int
	Close() error
}

// gattTransport writes ESC/POS bytes to the common BLE thermal-printer
// service 0x18f0 / characteristic 0x2af1. BlueZ performs ATT framing; payloads
// are limited to MTU-3 and lightly paced to avoid overrunning cheap printers
// when the characteristic uses write-without-response.
type gattTransport struct {
	cfg       config.PrinterConfig
	connector gattConnector

	mu           sync.Mutex
	connection   gattConnection
	writeTimeout time.Duration
	chunkDelay   time.Duration
}

func newGATTTransport(cfg config.PrinterConfig, connector gattConnector) *gattTransport {
	return &gattTransport{
		cfg: cfg, connector: connector, writeTimeout: gattWriteTimeout, chunkDelay: gattChunkDelay,
	}
}

func (t *gattTransport) Endpoint() string {
	address, err := configAddress(t.cfg)
	if err != nil {
		return t.cfg.Endpoint
	}
	return bleEndpoint(address)
}

func (t *gattTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	if t.connection != nil {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	if t.connector == nil {
		return errors.New("BLE GATT connector is unavailable")
	}
	address, err := configAddress(t.cfg)
	if err != nil {
		return err
	}
	connection, err := t.connector.ConnectGATT(ctx, address)
	if err != nil {
		return fmt.Errorf("connect %s: %w", bleEndpoint(address), err)
	}
	t.mu.Lock()
	if t.connection != nil {
		t.mu.Unlock()
		_ = connection.Close()
		return nil
	}
	t.connection = connection
	t.mu.Unlock()
	return nil
}

func (t *gattTransport) Write(ctx context.Context, data []byte) error {
	t.mu.Lock()
	connection := t.connection
	t.mu.Unlock()
	if connection == nil {
		return &transport.WriteError{BytesWritten: 0, Outcome: transport.WriteNotSent, Err: fmt.Errorf("%s: not connected", t.Endpoint())}
	}
	chunkSize := connection.MTU() - 3
	if chunkSize < 20 {
		chunkSize = 20
	}
	if chunkSize > 512 {
		chunkSize = 512
	}
	written := 0
	for written < len(data) {
		end := min(written+chunkSize, len(data))
		writeCtx, cancel := context.WithTimeout(ctx, t.writeTimeout)
		err := connection.WriteValue(writeCtx, data[written:end])
		timedOut := errors.Is(writeCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if err != nil {
			_ = t.Close()
			if timedOut {
				err = transport.ErrWriteTimeout
			} else if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				// Once a D-Bus write is in flight, cancellation cannot prove the
				// printer received zero bytes. Classify it like a timeout so the
				// Print Run is never automatically duplicated.
				err = fmt.Errorf("%w: %v", transport.ErrWriteTimeout, ctx.Err())
			}
			outcome := transport.WriteNotSent
			if written > 0 || timedOut || ctx.Err() != nil {
				outcome = transport.WriteAmbiguous
			}
			return &transport.WriteError{BytesWritten: written, Outcome: outcome, Err: fmt.Errorf("write %s: %w", t.Endpoint(), err)}
		}
		written = end
		if written < len(data) && t.chunkDelay > 0 {
			select {
			case <-time.After(t.chunkDelay):
			case <-ctx.Done():
				_ = t.Close()
				return &transport.WriteError{BytesWritten: written, Outcome: transport.WriteAmbiguous, Err: ctx.Err()}
			}
		}
	}
	return nil
}

func (t *gattTransport) Probe(context.Context) error { return transport.ErrUnsupported }

func (t *gattTransport) Close() error {
	t.mu.Lock()
	connection := t.connection
	t.connection = nil
	t.mu.Unlock()
	if connection == nil {
		return nil
	}
	return connection.Close()
}
