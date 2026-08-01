package transport

import (
	"context"
	"errors"
	"sync"
)

// MockTransport is an in-process fake printer for development and tests.
// It records everything written and supports scripted failures.
type MockTransport struct {
	endpoint string

	mu        sync.Mutex
	connected bool
	writes    [][]byte

	// Failure injection. All optional.
	FailConnect  error // returned by Connect
	FailWrite    error // returned by Write (wrapped in *WriteError)
	FailAfter    int   // bytes accepted before FailWrite triggers (0 = fail immediately)
	HangOnWrite  bool  // block until ctx is done, simulating a stalled link
	failWriteOnce bool
	// OnWrite, when set, runs after a successful write completes — lets
	// tests change state at the exact moment bytes have been "accepted".
	OnWrite func([]byte)
}

func NewMock(endpoint string) *MockTransport {
	if endpoint == "" {
		endpoint = "mock"
	}
	return &MockTransport{endpoint: endpoint}
}

func (m *MockTransport) Endpoint() string { return m.endpoint }

func (m *MockTransport) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailConnect != nil {
		return m.FailConnect
	}
	m.connected = true
	return nil
}

func (m *MockTransport) Write(ctx context.Context, data []byte) error {
	m.mu.Lock()
	if !m.connected {
		m.mu.Unlock()
		return &WriteError{Err: errors.New("mock: not connected")}
	}
	if m.HangOnWrite {
		m.mu.Unlock()
		<-ctx.Done()
		m.Close()
		return &WriteError{BytesWritten: 0, Err: ErrWriteTimeout}
	}
	if m.FailWrite != nil {
		n := min(m.FailAfter, len(data))
		m.writes = append(m.writes, append([]byte(nil), data[:n]...))
		err := m.FailWrite
		if m.failWriteOnce {
			m.FailWrite = nil
		}
		m.connected = false
		m.mu.Unlock()
		return &WriteError{BytesWritten: n, Err: err}
	}
	m.writes = append(m.writes, append([]byte(nil), data...))
	hook := m.OnWrite
	m.mu.Unlock()
	if hook != nil {
		hook(data)
	}
	return nil
}

// FailNextWrite arms a one-shot write failure after n accepted bytes.
func (m *MockTransport) FailNextWrite(err error, afterBytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FailWrite = err
	m.FailAfter = afterBytes
	m.failWriteOnce = true
}

func (m *MockTransport) Probe(ctx context.Context) error { return ErrUnsupported }

func (m *MockTransport) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = false
	return nil
}

// Writes returns a copy of everything written so far.
func (m *MockTransport) Writes() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.writes))
	copy(out, m.writes)
	return out
}
