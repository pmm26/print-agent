package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.bug.st/serial"

	"print-agent/internal/config"
)

const (
	// writeChunkSize keeps individual OS writes small so a stalled link is
	// detected quickly and BytesWritten stays accurate.
	writeChunkSize = 512
	// chunkTimeout is the deadline for one chunk to be accepted by the OS.
	// Bluetooth SPP is slow (~10-20 KB/s) but a healthy link accepts 512
	// bytes well within this.
	chunkTimeout = 10 * time.Second
)

// SerialTransport talks to a printer through an OS serial endpoint
// (Windows COM port and macOS /dev/cu.*). Platforms that own Bluetooth
// sockets directly can substitute their own transport.
//
// go.bug.st/serial exposes no portable write deadline, so writes run in a
// goroutine guarded by a watchdog: on timeout the port is closed, which
// unblocks the writer, and ErrWriteTimeout is reported.
type SerialTransport struct {
	cfg config.PrinterConfig

	mu   sync.Mutex
	port serial.Port
}

func NewSerial(cfg config.PrinterConfig) *SerialTransport {
	return &SerialTransport{cfg: cfg}
}

func (t *SerialTransport) Endpoint() string { return t.cfg.Endpoint }

func (t *SerialTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.port != nil {
		return nil
	}
	mode := &serial.Mode{
		BaudRate: t.cfg.BaudRate,
		DataBits: t.cfg.DataBits,
	}
	switch t.cfg.StopBits {
	case 2:
		mode.StopBits = serial.TwoStopBits
	default:
		mode.StopBits = serial.OneStopBit
	}
	switch t.cfg.Parity {
	case config.ParityOdd:
		mode.Parity = serial.OddParity
	case config.ParityEven:
		mode.Parity = serial.EvenParity
	default:
		mode.Parity = serial.NoParity
	}

	// serial.Open itself can block while the OS (re)establishes the
	// Bluetooth link, so it runs under the same watchdog pattern as writes.
	type openResult struct {
		port serial.Port
		err  error
	}
	ch := make(chan openResult, 1)
	go func() {
		p, err := serial.Open(t.cfg.Endpoint, mode)
		ch <- openResult{p, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return fmt.Errorf("open %s: %w", t.cfg.Endpoint, res.err)
		}
		t.port = res.port
		return nil
	case <-time.After(30 * time.Second):
		// Abandon the hung open; if it eventually succeeds the goroutine
		// closes the orphaned port.
		go func() {
			if res := <-ch; res.err == nil {
				res.port.Close()
			}
		}()
		return fmt.Errorf("open %s: %w", t.cfg.Endpoint, ErrWriteTimeout)
	case <-ctx.Done():
		go func() {
			if res := <-ch; res.err == nil {
				res.port.Close()
			}
		}()
		return ctx.Err()
	}
}

func (t *SerialTransport) Write(ctx context.Context, data []byte) error {
	t.mu.Lock()
	port := t.port
	t.mu.Unlock()
	if port == nil {
		return &WriteError{BytesWritten: 0, Outcome: WriteNotSent, Err: fmt.Errorf("%s: not connected", t.cfg.Endpoint)}
	}

	written := 0
	for written < len(data) {
		end := min(written+writeChunkSize, len(data))
		n, err := t.writeChunk(ctx, port, data[written:end])
		written += n
		if err != nil {
			t.Close()
			outcome := WriteNotSent
			if written > 0 || errors.Is(err, ErrWriteTimeout) || errors.Is(err, context.Canceled) {
				outcome = WriteAmbiguous
			}
			return &WriteError{BytesWritten: written, Outcome: outcome, Err: err}
		}
	}
	return nil
}

// writeChunk performs one guarded write. On timeout it closes the port to
// unblock the writer goroutine; the number of bytes the OS accepted is then
// unknowable, so it reports the chunk as partially written (n=0 for the
// chunk, but the caller has already counted prior chunks).
func (t *SerialTransport) writeChunk(ctx context.Context, port serial.Port, chunk []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := port.Write(chunk)
		ch <- result{n, err}
	}()
	timer := time.NewTimer(chunkTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			return res.n, fmt.Errorf("write %s: %w", t.cfg.Endpoint, res.err)
		}
		if res.n < len(chunk) {
			return res.n, fmt.Errorf("short write to %s (%d/%d)", t.cfg.Endpoint, res.n, len(chunk))
		}
		return res.n, nil
	case <-timer.C:
		port.Close() // unblocks the writer
		boundedReap(ch)
		return 0, ErrWriteTimeout
	case <-ctx.Done():
		port.Close()
		boundedReap(ch)
		return 0, ctx.Err()
	}
}

func boundedReap[T any](ch <-chan T) {
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
	}
}

// Probe drains nothing and sends nothing by default: generic BT printers
// give no safe universal status query, so probing is unsupported unless
// enabled per-printer with a validated command (future work).
func (t *SerialTransport) Probe(ctx context.Context) error { return ErrUnsupported }

func (t *SerialTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.port == nil {
		return nil
	}
	err := t.port.Close()
	t.port = nil
	return err
}
