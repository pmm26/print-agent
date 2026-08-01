package printers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"print-agent/internal/bluetooth"
	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/transport"
)

// maxWriteAttempts bounds automatic retries of a delivery whose writes fail
// cleanly (zero bytes). After this many claims the delivery is failed for
// the operator to handle.
const maxWriteAttempts = 5

// queuePollInterval is the safety net for missed wake signals: the worker
// re-checks SQLite this often while connected and idle.
const queuePollInterval = 5 * time.Second

type worker struct {
	cfg          config.PrinterConfig
	repo         *jobs.Repository
	renderer     *escpos.Renderer
	connector    bluetooth.Connector
	bus          *events.Bus
	newTransport func(config.PrinterConfig) transport.Transport

	wake         chan struct{}
	reconnectNow chan struct{}
	cancel       context.CancelFunc
	done         chan struct{}

	mu        sync.Mutex
	state     ConnectionState
	tr        transport.Transport
	lastError string
	lastTx    *time.Time
	attempt   int
	nextRetry *time.Time
}

func newWorker(cfg config.PrinterConfig, repo *jobs.Repository, renderer *escpos.Renderer,
	connector bluetooth.Connector, bus *events.Bus,
	factory func(config.PrinterConfig) transport.Transport) *worker {
	return &worker{
		cfg:          cfg,
		repo:         repo,
		renderer:     renderer,
		connector:    connector,
		bus:          bus,
		newTransport: factory,
		wake:         make(chan struct{}, 1),
		reconnectNow: make(chan struct{}, 1),
		done:         make(chan struct{}),
		state:        StateDisconnected,
	}
}

func (w *worker) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	go w.run(ctx)
}

func (w *worker) stop() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}
}

// Wake nudges the worker; never blocks (channel has capacity 1 and a missed
// signal is covered by the periodic queue poll).
func (w *worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// ReconnectNow interrupts any backoff wait or idle wait and forces an
// immediate reconnect cycle.
func (w *worker) ReconnectNow() {
	select {
	case w.reconnectNow <- struct{}{}:
	default:
	}
}

func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	defer w.closeTransport(StateDisconnected, "worker stopped")

	for ctx.Err() == nil {
		if !w.connectLoop(ctx) {
			return
		}
		w.drainQueue(ctx)
		if w.snapshotState() != StateConnected {
			continue // connection was lost while printing; reconnect
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-time.After(queuePollInterval):
		case <-w.reconnectNow:
			w.closeTransport(StateDisconnected, "manual reconnect")
		}
	}
}

// connectLoop blocks until the printer is connected (true) or the context is
// cancelled (false), applying the spec's backoff between attempts. When the
// worker is already connected it returns immediately — reopening a serial
// endpoint we still hold would fail with "port busy".
func (w *worker) connectLoop(ctx context.Context) bool {
	w.mu.Lock()
	alreadyConnected := w.tr != nil && w.state == StateConnected
	w.mu.Unlock()
	if alreadyConnected {
		// The endpoint being open proves little on macOS; confirm the OS
		// still reports the Bluetooth link up.
		if err := w.connector.VerifyConnected(ctx, w.cfg); err == nil {
			return true
		}
		w.closeTransport(StateDisconnected, "bluetooth link lost")
		w.bus.Publish(events.Event{Type: events.PrinterDisconnected, PrinterID: w.cfg.ID,
			Message: "OS reports the printer disconnected"})
	}
	attempt := 0
	for ctx.Err() == nil {
		if attempt == 0 {
			w.setState(StateConnecting)
		} else {
			w.setState(StateReconnecting)
		}
		err := w.connectOnce(ctx)
		if err == nil {
			w.mu.Lock()
			w.state = StateConnected
			w.lastError = ""
			w.attempt = 0
			w.nextRetry = nil
			endpoint := w.tr.Endpoint()
			w.mu.Unlock()
			w.bus.Publish(events.Event{Type: events.PrinterConnected, PrinterID: w.cfg.ID, Message: endpoint})
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		attempt++
		delay := reconnectDelay(attempt)
		next := time.Now().Add(delay)
		w.mu.Lock()
		w.lastError = err.Error()
		w.attempt = attempt
		w.nextRetry = &next
		manualOnly := !w.cfg.AutoReconnect
		if manualOnly {
			w.state = StateError
			w.nextRetry = nil
		}
		w.mu.Unlock()
		if attempt == 1 {
			w.bus.Publish(events.Event{Type: events.PrinterError, PrinterID: w.cfg.ID, Message: err.Error()})
		}

		if manualOnly {
			// Park until an operator presses Reconnect.
			select {
			case <-ctx.Done():
				return false
			case <-w.reconnectNow:
				continue
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		case <-w.reconnectNow:
		}
	}
	return false
}

func (w *worker) connectOnce(ctx context.Context) error {
	cfg := w.cfg
	// Only real Bluetooth serial endpoints need the platform connector; the
	// mock transport exists purely in-process.
	if cfg.Transport == config.TransportBluetoothSerial {
		endpoint, err := w.connector.EnsureConnected(ctx, w.cfg)
		if err != nil {
			return err
		}
		cfg.Endpoint = endpoint
	}
	tr := w.newTransport(cfg)
	if err := tr.Connect(ctx); err != nil {
		return err
	}
	// Opening the endpoint succeeds on macOS even when the printer is off.
	// Confirm the OS-level Bluetooth link; the open may itself trigger the
	// link to come up, so allow one short grace retry.
	if err := w.connector.VerifyConnected(ctx, cfg); err != nil {
		select {
		case <-time.After(4 * time.Second): // outlive the connector's state cache
		case <-ctx.Done():
			tr.Close()
			return ctx.Err()
		}
		if err := w.connector.VerifyConnected(ctx, cfg); err != nil {
			tr.Close()
			return fmt.Errorf("endpoint %s opened but %w — is the printer on?", cfg.Endpoint, err)
		}
	}
	w.mu.Lock()
	w.tr = tr
	w.mu.Unlock()
	return nil
}

// drainQueue processes queued deliveries until the queue is empty, the
// connection drops, or the context ends.
func (w *worker) drainQueue(ctx context.Context) {
	for ctx.Err() == nil && w.snapshotState() == StateConnected {
		delivery, err := w.repo.ClaimNextQueued(w.cfg.ID)
		if errors.Is(err, jobs.ErrNotFound) {
			return
		}
		if err != nil {
			w.bus.Publish(events.Event{Type: events.PrinterError, PrinterID: w.cfg.ID,
				Message: "queue claim failed: " + err.Error()})
			return
		}
		w.bus.Publish(events.Event{Type: events.DeliveryProcessing, PrinterID: w.cfg.ID,
			DeliveryID: delivery.ExternalDeliveryID})
		w.process(ctx, delivery)
	}
}

func (w *worker) process(ctx context.Context, d jobs.Delivery) {
	w.setState(StatePrinting)

	doc, err := w.renderer.Render(d.Template, d.PayloadJSON, w.cfg,
		escpos.RenderOptions{Reprint: d.ReprintOf != ""})
	if err != nil {
		// Rendering is deterministic; retrying cannot help.
		w.repo.MarkFailed(d.ID, 0, "render: "+err.Error())
		w.bus.Publish(events.Event{Type: events.DeliveryFailed, PrinterID: w.cfg.ID,
			DeliveryID: d.ExternalDeliveryID, Message: err.Error()})
		w.setState(StateConnected)
		return
	}

	w.mu.Lock()
	tr := w.tr
	w.mu.Unlock()
	err = tr.Write(ctx, doc)
	if err == nil {
		// The OS accepted every byte, but on macOS that only means they were
		// buffered. Claim `transmitted` only while the Bluetooth link is
		// confirmed up; otherwise the ticket's fate is unknowable.
		if verr := w.connector.VerifyConnected(ctx, w.cfg); verr != nil {
			w.closeTransport(StateDisconnected, verr.Error())
			w.repo.MarkUncertain(d.ID, len(doc),
				"bytes accepted by the OS but the printer is not connected")
			w.bus.Publish(events.Event{Type: events.DeliveryUncertain, PrinterID: w.cfg.ID,
				DeliveryID: d.ExternalDeliveryID, Message: "printer disconnected during transmission"})
			w.bus.Publish(events.Event{Type: events.PrinterDisconnected, PrinterID: w.cfg.ID,
				Message: "OS reports the printer disconnected"})
			return
		}
		now := time.Now().UTC()
		w.repo.MarkTransmitted(d.ID, len(doc))
		w.mu.Lock()
		w.lastTx = &now
		w.state = StateConnected
		w.mu.Unlock()
		w.bus.Publish(events.Event{Type: events.DeliveryTransmitted, PrinterID: w.cfg.ID,
			DeliveryID: d.ExternalDeliveryID, Message: fmt.Sprintf("%d bytes", len(doc))})
		return
	}

	// Any write error means the connection can no longer be trusted.
	bytesWritten := 0
	if werr, ok := errors.AsType[*transport.WriteError](err); ok {
		bytesWritten = werr.BytesWritten
	}
	w.closeTransport(StateDisconnected, err.Error())
	w.bus.Publish(events.Event{Type: events.PrinterDisconnected, PrinterID: w.cfg.ID, Message: err.Error()})

	// A timed-out write may have partially reached the printer even when the
	// counted bytes are zero, so timeouts are always uncertain.
	uncertain := bytesWritten > 0 || errors.Is(err, transport.ErrWriteTimeout)
	switch {
	case uncertain:
		w.repo.MarkUncertain(d.ID, bytesWritten, err.Error())
		w.bus.Publish(events.Event{Type: events.DeliveryUncertain, PrinterID: w.cfg.ID,
			DeliveryID: d.ExternalDeliveryID, Message: err.Error()})
	case d.AttemptCount < maxWriteAttempts:
		w.repo.Requeue(d.ID, err.Error())
	default:
		w.repo.MarkFailed(d.ID, 0, err.Error())
		w.bus.Publish(events.Event{Type: events.DeliveryFailed, PrinterID: w.cfg.ID,
			DeliveryID: d.ExternalDeliveryID, Message: err.Error()})
	}
}

func (w *worker) closeTransport(state ConnectionState, reason string) {
	w.mu.Lock()
	if w.tr != nil {
		w.tr.Close()
		w.tr = nil
	}
	w.state = state
	if reason != "" {
		w.lastError = reason
	}
	w.mu.Unlock()
}

func (w *worker) setState(s ConnectionState) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}

func (w *worker) snapshotState() ConnectionState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *worker) status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	endpoint := w.cfg.Endpoint
	if w.tr != nil {
		endpoint = w.tr.Endpoint()
	}
	return Status{
		Printer:          w.cfg,
		State:            w.state,
		Endpoint:         endpoint,
		LastError:        w.lastError,
		LastTransmission: w.lastTx,
		ReconnectAttempt: w.attempt,
		NextRetryAt:      w.nextRetry,
	}
}
