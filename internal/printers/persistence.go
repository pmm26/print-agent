package printers

import (
	"context"
	"sync"
	"time"
)

type persistenceStatus struct {
	Paused bool       `json:"paused"`
	Reason string     `json:"reason,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
}

// persistenceGate stops new physical side effects while one or more workers
// are retrying a Print Run transition that SQLite did not accept.
type persistenceGate struct {
	mu       sync.Mutex
	failures int
	reason   string
	since    *time.Time
	resume   chan struct{}
}

func newPersistenceGate() *persistenceGate {
	return &persistenceGate{resume: make(chan struct{})}
}

func (g *persistenceGate) wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.failures == 0 {
			g.mu.Unlock()
			return nil
		}
		resume := g.resume
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resume:
		}
	}
}

func (g *persistenceGate) begin(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures++
	if g.failures == 1 {
		now := time.Now().UTC()
		g.since = &now
		g.resume = make(chan struct{})
	}
	g.reason = err.Error()
}

func (g *persistenceGate) update(err error) {
	g.mu.Lock()
	g.reason = err.Error()
	g.mu.Unlock()
}

func (g *persistenceGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failures > 0 {
		g.failures--
	}
	if g.failures == 0 {
		g.reason = ""
		g.since = nil
		close(g.resume)
	}
}

func (g *persistenceGate) status() persistenceStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return persistenceStatus{Paused: g.failures > 0, Reason: g.reason, Since: g.since}
}
