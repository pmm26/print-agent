package events

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder isolates operational event persistence from printer and HTTP paths.
// Domain events use Store.AppendTx and never pass through this bounded queue.
type Recorder struct {
	store   *Store
	queue   chan Event
	dropped atomic.Uint64

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewRecorder(store *Store, capacity int) *Recorder {
	if capacity <= 0 {
		capacity = 2048
	}
	return &Recorder{store: store, queue: make(chan Event, capacity)}
}

func (r *Recorder) Start(parent context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.run(ctx)
}

func (r *Recorder) Publish(event Event) bool {
	event.Durability = DurabilityOperational
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	select {
	case r.queue <- event:
		return true
	default:
		r.dropped.Add(1)
		return false
	}
}

func (r *Recorder) Dropped() uint64 { return r.dropped.Load() }

func (r *Recorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel = nil
	r.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Recorder) run(ctx context.Context) {
	defer close(r.done)
	var pending *Event
	for {
		if pending != nil {
			if _, err := r.store.Append(context.Background(), *pending); err != nil {
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
					continue
				}
			}
			pending = nil
			continue
		}
		select {
		case event := <-r.queue:
			pending = &event
		case <-ctx.Done():
			flush := time.NewTimer(2 * time.Second)
			defer flush.Stop()
			for {
				select {
				case event := <-r.queue:
					_, _ = r.store.Append(context.Background(), event)
				case <-flush.C:
					return
				default:
					return
				}
			}
		}
	}
}
