//go:build linux

package linux

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/transport"
)

type fakeRFCOMMSocket struct {
	mu        sync.Mutex
	writes    [][]byte
	writeFunc func([]byte) (int, error)
	closeOnce sync.Once
	closed    chan struct{}
}

func newFakeRFCOMMSocket() *fakeRFCOMMSocket {
	return &fakeRFCOMMSocket{closed: make(chan struct{})}
}
func (s *fakeRFCOMMSocket) Write(data []byte) (int, error) {
	if s.writeFunc != nil {
		return s.writeFunc(data)
	}
	s.mu.Lock()
	s.writes = append(s.writes, append([]byte(nil), data...))
	s.mu.Unlock()
	return len(data), nil
}
func (s *fakeRFCOMMSocket) SetWriteDeadline(time.Time) error { return nil }
func (s *fakeRFCOMMSocket) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func TestRFCOMMTransportChunksWrites(t *testing.T) {
	socket := newFakeRFCOMMSocket()
	connector := &fakeProfileConnector{connect: func(context.Context, string) (rfcommSocket, error) {
		return socket, nil
	}}
	tr := newRFCOMMTransport(config.PrinterConfig{Endpoint: "rfcomm://AA:BB:CC:DD:EE:FF"}, connector)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tr.Write(context.Background(), make([]byte, 1200)); err != nil {
		t.Fatal(err)
	}
	socket.mu.Lock()
	defer socket.mu.Unlock()
	if len(socket.writes) != 3 || len(socket.writes[0]) != 512 || len(socket.writes[1]) != 512 || len(socket.writes[2]) != 176 {
		t.Fatalf("chunk lengths = %d/%d/%d (writes=%d)", len(socket.writes[0]), len(socket.writes[1]), len(socket.writes[2]), len(socket.writes))
	}
}

func TestRFCOMMTransportReportsPartialWrite(t *testing.T) {
	socket := newFakeRFCOMMSocket()
	socket.writeFunc = func([]byte) (int, error) { return 3, errors.New("link lost") }
	connector := &fakeProfileConnector{connect: func(context.Context, string) (rfcommSocket, error) {
		return socket, nil
	}}
	tr := newRFCOMMTransport(config.PrinterConfig{DeviceAddress: "AA:BB:CC:DD:EE:FF"}, connector)
	_ = tr.Connect(context.Background())
	err := tr.Write(context.Background(), []byte("abcdef"))
	var writeErr *transport.WriteError
	if !errors.As(err, &writeErr) || writeErr.BytesWritten != 3 {
		t.Fatalf("Write error = %#v", err)
	}
	select {
	case <-socket.closed:
	default:
		t.Fatal("failed write did not close RFCOMM socket")
	}
}

func TestRFCOMMTransportWriteTimeout(t *testing.T) {
	socket := newFakeRFCOMMSocket()
	socket.writeFunc = func([]byte) (int, error) {
		<-socket.closed
		return 0, errors.New("closed")
	}
	connector := &fakeProfileConnector{connect: func(context.Context, string) (rfcommSocket, error) {
		return socket, nil
	}}
	tr := newRFCOMMTransport(config.PrinterConfig{DeviceAddress: "AA:BB:CC:DD:EE:FF"}, connector)
	tr.chunkTimeout = 20 * time.Millisecond
	_ = tr.Connect(context.Background())
	start := time.Now()
	err := tr.Write(context.Background(), []byte("blocked"))
	if !errors.Is(err, transport.ErrWriteTimeout) {
		t.Fatalf("Write = %v, want ErrWriteTimeout", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("write timeout did not abort promptly")
	}
}

func TestRFCOMMTransportContextCancellation(t *testing.T) {
	socket := newFakeRFCOMMSocket()
	socket.writeFunc = func([]byte) (int, error) {
		<-socket.closed
		return 0, errors.New("closed")
	}
	connector := &fakeProfileConnector{connect: func(context.Context, string) (rfcommSocket, error) {
		return socket, nil
	}}
	tr := newRFCOMMTransport(config.PrinterConfig{DeviceAddress: "AA:BB:CC:DD:EE:FF"}, connector)
	_ = tr.Connect(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr.Write(ctx, []byte("blocked")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Write = %v, want context.Canceled", err)
	} else {
		var writeErr *transport.WriteError
		if !errors.As(err, &writeErr) || !writeErr.Ambiguous() {
			t.Fatalf("Write = %#v, want ambiguous WriteError", err)
		}
	}
}

func TestRFCOMMTransportConnectIsIdempotentAndReconnectsAfterClose(t *testing.T) {
	connects := 0
	connector := &fakeProfileConnector{connect: func(context.Context, string) (rfcommSocket, error) {
		connects++
		return newFakeRFCOMMSocket(), nil
	}}
	tr := newRFCOMMTransport(config.PrinterConfig{Endpoint: "rfcomm://AA:BB:CC:DD:EE:FF"}, connector)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if connects != 1 {
		t.Fatalf("connects = %d, want 1", connects)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if connects != 2 {
		t.Fatalf("connects after reconnect = %d, want 2", connects)
	}
}
