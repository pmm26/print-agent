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
	rfcommWriteChunkSize = 512
	rfcommChunkTimeout   = 10 * time.Second
)

type rfcommSocket interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
	Close() error
}

type rfcommTransport struct {
	cfg       config.PrinterConfig
	connector profileConnector

	mu           sync.Mutex
	socket       rfcommSocket
	chunkTimeout time.Duration
}

func newRFCOMMTransport(cfg config.PrinterConfig, connector profileConnector) *rfcommTransport {
	return &rfcommTransport{cfg: cfg, connector: connector, chunkTimeout: rfcommChunkTimeout}
}

func (t *rfcommTransport) Endpoint() string {
	address, err := configAddress(t.cfg)
	if err != nil {
		return t.cfg.Endpoint
	}
	return rfcommEndpoint(address)
}

func (t *rfcommTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	if t.socket != nil {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	address, err := configAddress(t.cfg)
	if err != nil {
		return err
	}
	socket, err := t.connector.Connect(ctx, address)
	if err != nil {
		return fmt.Errorf("connect %s: %w", rfcommEndpoint(address), err)
	}
	t.mu.Lock()
	if t.socket != nil {
		t.mu.Unlock()
		_ = socket.Close()
		return nil
	}
	t.socket = socket
	t.mu.Unlock()
	return nil
}

func (t *rfcommTransport) Write(ctx context.Context, data []byte) error {
	t.mu.Lock()
	socket := t.socket
	t.mu.Unlock()
	if socket == nil {
		return &transport.WriteError{BytesWritten: 0, Outcome: transport.WriteNotSent, Err: fmt.Errorf("%s: not connected", t.Endpoint())}
	}
	written := 0
	for written < len(data) {
		end := min(written+rfcommWriteChunkSize, len(data))
		n, err := t.writeChunk(ctx, socket, data[written:end])
		written += n
		if err != nil {
			_ = t.Close()
			outcome := transport.WriteNotSent
			if written > 0 || errors.Is(err, transport.ErrWriteTimeout) || errors.Is(err, context.Canceled) {
				outcome = transport.WriteAmbiguous
			}
			return &transport.WriteError{BytesWritten: written, Outcome: outcome, Err: err}
		}
	}
	return nil
}

func (t *rfcommTransport) writeChunk(ctx context.Context, socket rfcommSocket, chunk []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	_ = socket.SetWriteDeadline(time.Now().Add(t.chunkTimeout))
	resultCh := make(chan result, 1)
	go func() {
		n, err := socket.Write(chunk)
		resultCh <- result{n: n, err: err}
	}()
	timer := time.NewTimer(t.chunkTimeout)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if result.err != nil {
			return result.n, fmt.Errorf("write %s: %w", t.Endpoint(), result.err)
		}
		if result.n != len(chunk) {
			return result.n, fmt.Errorf("short write to %s (%d/%d)", t.Endpoint(), result.n, len(chunk))
		}
		return result.n, nil
	case <-ctx.Done():
		_ = socket.Close()
		boundedSocketReap(resultCh)
		return 0, ctx.Err()
	case <-timer.C:
		_ = socket.Close()
		boundedSocketReap(resultCh)
		return 0, transport.ErrWriteTimeout
	}
}

func boundedSocketReap[T any](ch <-chan T) {
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
	}
}

func (t *rfcommTransport) Probe(context.Context) error { return transport.ErrUnsupported }

func (t *rfcommTransport) Close() error {
	t.mu.Lock()
	socket := t.socket
	t.socket = nil
	t.mu.Unlock()
	if socket == nil {
		return nil
	}
	return socket.Close()
}
