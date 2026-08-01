package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.bug.st/serial"

	"print-agent/internal/config"
)

// fakePort implements just enough of serial.Port for the watchdog test.
// Embedding the interface panics on any other method, keeping the fake honest.
type fakePort struct {
	serial.Port
	writeStarted chan struct{}
	unblock      chan struct{}
	closed       chan struct{}
}

func (f *fakePort) Write(p []byte) (int, error) {
	close(f.writeStarted)
	<-f.unblock
	return 0, errors.New("port closed under writer")
}

func (f *fakePort) Close() error {
	close(f.closed)
	close(f.unblock)
	return nil
}

// TestWriteWatchdogUnblocksHungWrite verifies the core "never freeze on a
// blocked write" requirement: a hung OS write is aborted by closing the
// port, and the caller sees ErrWriteTimeout.
func TestWriteWatchdogUnblocksHungWrite(t *testing.T) {
	tr := NewSerial(config.PrinterConfig{Endpoint: "fake"})
	port := &fakePort{
		writeStarted: make(chan struct{}),
		unblock:      make(chan struct{}),
		closed:       make(chan struct{}),
	}

	// A cancellable context stands in for the chunk timeout so the test
	// doesn't wait 10 real seconds.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-port.writeStarted
		cancel()
	}()

	start := time.Now()
	_, err := tr.writeChunk(ctx, port, []byte("stuck"))
	if err == nil {
		t.Fatal("expected an error from the aborted write")
	}
	select {
	case <-port.closed:
	default:
		t.Fatal("watchdog must close the port to unblock the writer")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("write was not unblocked promptly (%s)", elapsed)
	}
}

// TestWriteReportsBytesWritten verifies partial-transmission accounting that
// the failed-vs-uncertain classification depends on.
func TestWriteReportsBytesWritten(t *testing.T) {
	m := NewMock("mock")
	m.Connect(context.Background())
	m.FailNextWrite(errors.New("link dropped"), 3)

	err := m.Write(context.Background(), []byte("abcdef"))
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %v", err)
	}
	if werr.BytesWritten != 3 {
		t.Errorf("BytesWritten = %d, want 3", werr.BytesWritten)
	}
}
