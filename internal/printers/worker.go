package printers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/platform"
	"print-agent/internal/transport"
)

// queuePollInterval is the safety net for missed wake signals: the worker
// re-checks SQLite this often while connected and idle.
const queuePollInterval = 5 * time.Second

type worker struct {
	cfg          config.PrinterConfig
	repo         *jobs.Repository
	renderer     *escpos.Renderer
	driver       platform.Driver
	bus          events.Publisher
	newTransport func(config.PrinterConfig) transport.Transport
	gate         *persistenceGate
	transmission *transmissionGate

	wake         chan struct{}
	reconnectNow chan struct{}
	cancel       context.CancelFunc
	done         chan struct{}

	mu        sync.Mutex
	state     ConnectionState
	activity  ActivityState
	tr        transport.Transport
	activeCfg config.PrinterConfig
	lastError string
	lastTx    *time.Time
	attempt   int
	nextRetry *time.Time
}

func newWorker(cfg config.PrinterConfig, repo *jobs.Repository, renderer *escpos.Renderer,
	driver platform.Driver, bus events.Publisher,
	factory func(config.PrinterConfig) transport.Transport) *worker {
	return &worker{
		cfg:          cfg,
		repo:         repo,
		renderer:     renderer,
		driver:       driver,
		bus:          bus,
		newTransport: factory,
		wake:         make(chan struct{}, 1),
		reconnectNow: make(chan struct{}, 1),
		done:         make(chan struct{}),
		state:        StateDisconnected,
		activity:     ActivityIdle,
		transmission: newTransmissionGate(),
	}
}

func (w *worker) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	go w.run(ctx)
}

func (w *worker) stop() {
	if w.cancel != nil {
		w.setState(StateStopping)
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
			continue // connection was lost during queue activity; reconnect
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
		if state, err := w.linkState(ctx, w.connectionConfig()); err == nil && state == platform.LinkConnected {
			w.materializeRetries()
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
			w.bus.Publish(events.Event{Type: events.PrinterConnecting, PrinterID: w.cfg.ID,
				Metadata: map[string]any{"connectionAttempt": 1, "reconnect": false}})
		} else {
			w.setState(StateConnecting)
			w.bus.Publish(events.Event{Type: events.PrinterConnecting, PrinterID: w.cfg.ID,
				Metadata: map[string]any{"connectionAttempt": attempt + 1, "reconnect": true}})
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
			w.bus.Publish(events.Event{Type: events.PrinterConnected, PrinterID: w.cfg.ID,
				Metadata: map[string]any{"endpoint": endpoint, "reconnect": attempt > 0,
					"connectionAttempt": attempt + 1}})
			w.materializeRetries()
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
		manualOnly := !w.cfg.AutoReconnect || requiresOperatorAction(err)
		if manualOnly {
			w.state = StateOperatorAction
			w.nextRetry = nil
		}
		w.mu.Unlock()
		w.bus.Publish(events.Event{Type: events.PrinterConnectionFailed, PrinterID: w.cfg.ID,
			Error:    &events.Error{Code: "connection_failed", Message: err.Error(), Retryable: !manualOnly},
			Metadata: map[string]any{"connectionAttempt": attempt}})

		if manualOnly {
			// Park until an operator presses Reconnect.
			select {
			case <-ctx.Done():
				return false
			case <-w.reconnectNow:
				continue
			}
		}
		w.setState(StateReconnectWait)
		w.bus.Publish(events.Event{Type: events.PrinterReconnectScheduled, PrinterID: w.cfg.ID,
			Metadata: map[string]any{"connectionAttempt": attempt + 1, "delayMs": delay.Milliseconds(), "nextRetryAt": next.UTC()}})
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		case <-w.reconnectNow:
		}
	}
	return false
}

func requiresOperatorAction(err error) bool {
	return errors.Is(err, platform.ErrBluetoothProtocolUnsupported) ||
		errors.Is(err, platform.ErrBluetoothNotAuthorized) ||
		errors.Is(err, platform.ErrInvalidBluetoothAddress)
}

func (w *worker) connectOnce(ctx context.Context) error {
	cfg := w.cfg
	// Only real Bluetooth serial endpoints need the platform driver's
	// endpoint resolution; the mock transport exists purely in-process.
	if cfg.Transport == config.TransportBluetoothSerial {
		endpoint, err := w.driver.EnsureConnected(ctx, w.cfg)
		if err != nil {
			return err
		}
		cfg.Endpoint = endpoint
	}
	tr := w.newTransport(cfg)
	if err := tr.Connect(ctx); err != nil {
		_ = tr.Close()
		return err
	}
	// Opening the endpoint succeeds on macOS even when the printer is off.
	// Confirm the OS-level Bluetooth link; the open may itself trigger the
	// link to come up, so allow one short grace retry.
	w.setState(StateVerifying)
	state, verifyErr := w.linkState(ctx, cfg)
	if verifyErr != nil || state != platform.LinkConnected {
		select {
		case <-time.After(4 * time.Second): // outlive the driver's state cache
		case <-ctx.Done():
			tr.Close()
			return ctx.Err()
		}
		state, verifyErr = w.linkState(ctx, cfg)
		if verifyErr != nil || state != platform.LinkConnected {
			tr.Close()
			if verifyErr != nil {
				return fmt.Errorf("verify endpoint %s: %w", cfg.Endpoint, verifyErr)
			}
			return fmt.Errorf("endpoint %s opened but Bluetooth link is %s", cfg.Endpoint, state)
		}
	}
	w.mu.Lock()
	w.tr = tr
	w.activeCfg = cfg
	w.mu.Unlock()
	return nil
}

// drainQueue processes queued Print Runs until the queue is empty, the
// connection drops, or the context ends.
func (w *worker) drainQueue(ctx context.Context) {
	if w.repo == nil {
		return
	}
	for ctx.Err() == nil && w.snapshotState() == StateConnected {
		if w.gate != nil {
			if err := w.gate.wait(ctx); err != nil {
				return
			}
		}
		if err := w.transmission.acquire(ctx); err != nil {
			return
		}
		run, err := w.claimNext(ctx)
		if errors.Is(err, jobs.ErrNotFound) {
			w.transmission.release()
			return
		}
		if err != nil {
			w.transmission.release()
			w.bus.Publish(events.Event{Type: events.PrinterError, PrinterID: w.cfg.ID,
				Message: "queue claim failed: " + err.Error()})
			return
		}
		w.setActivity(ActivityClaimed)
		w.process(ctx, run)
		w.setActivity(ActivityIdle)
		w.transmission.release()
	}
}

func (w *worker) claimNext(ctx context.Context) (jobs.PrintRun, error) {
	run, err := w.repo.ClaimNextQueued(w.cfg.ID)
	if err == nil || errors.Is(err, jobs.ErrNotFound) {
		return run, err
	}
	if w.gate == nil {
		w.gate = newPersistenceGate()
	}
	w.gate.begin(err)
	if w.bus != nil {
		w.bus.Publish(events.Event{Type: events.PersistenceDegraded,
			Error: &events.Error{Code: "sqlite_unavailable", Message: "delivery-state persistence is unavailable", Retryable: true}})
	}
	delay := 250 * time.Millisecond
	for {
		w.mu.Lock()
		w.lastError = "database persistence paused: " + err.Error()
		w.mu.Unlock()
		if !waitContext(ctx, delay) {
			return jobs.PrintRun{}, ctx.Err()
		}
		run, err = w.repo.ClaimNextQueued(w.cfg.ID)
		if err == nil || errors.Is(err, jobs.ErrNotFound) {
			if w.gate.end() {
				w.bus.Publish(events.Event{Type: events.PersistenceRecovered})
			}
			w.mu.Lock()
			w.lastError = ""
			w.mu.Unlock()
			return run, err
		}
		w.gate.update(err)
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (w *worker) materializeRetries() {
	if w.repo == nil || w.renderer == nil {
		return
	}
	runs, err := w.repo.MaterializeRetries(w.cfg.ID, jobs.MaxAutomaticRetries,
		func(job jobs.Job, target jobs.JobPrinter, previous jobs.PrintRun, runNumber int) (string, error) {
			return jobs.ExpectedContentHash(w.renderer, job, target, previous.ContentMode, runNumber)
		})
	_ = runs // repository commits retry events atomically with retry creation
	if err != nil {
		w.bus.Publish(events.Event{Type: events.PrinterError, PrinterID: w.cfg.ID,
			Message: "retry creation failed: " + err.Error()})
	}
}

func (w *worker) process(ctx context.Context, run jobs.PrintRun) {
	job, err := w.repo.GetJob(run.JobUID)
	if err != nil {
		w.persist(ctx, func() error { return w.repo.MarkFailed(run.UID, 0, "job_unavailable", err.Error(), false) })
		return
	}
	target, err := w.repo.GetTarget(run.JobUID, run.PrinterID)
	if err != nil {
		w.persist(ctx, func() error { return w.repo.MarkFailed(run.UID, 0, "target_unavailable", err.Error(), false) })
		return
	}
	renderCfg := w.cfg
	renderCfg.Encoding = target.Encoding
	renderCfg.CharactersPerLine = target.CharactersPerLine
	doc, err := w.renderer.Render(job.Template, job.Data, renderCfg, escpos.RenderOptions{
		Reprint: run.ContentMode == jobs.ContentReprint, RunNumber: run.RunNumber, AcceptedAt: job.CreatedAt,
	})
	if err != nil {
		w.persist(ctx, func() error { return w.repo.MarkFailed(run.UID, 0, "render_failed", err.Error(), false) })
		return
	}
	expected, err := jobs.ExpectedContentHash(w.renderer, job, target, run.ContentMode, run.RunNumber)
	if err != nil || expected != run.ExpectedContentHash || jobs.ContentHash(doc) != run.ExpectedContentHash {
		message := "rendered content no longer matches accepted content"
		if err != nil {
			message = err.Error()
		}
		w.persist(ctx, func() error { return w.repo.MarkFailed(run.UID, 0, "content_hash_mismatch", message, false) })
		return
	}
	if ctx.Err() != nil {
		w.persist(ctx, func() error {
			return w.repo.MarkFailed(run.UID, 0, "shutdown_before_transmission", "agent stopped before transmission began", true)
		})
		return
	}
	if !w.persist(ctx, func() error { return w.repo.MarkTransmitting(run.UID) }) {
		return
	}
	w.setActivity(ActivityTransmitting)

	w.mu.Lock()
	tr := w.tr
	activeCfg := w.activeCfg
	w.mu.Unlock()
	err = tr.Write(ctx, doc)
	if err == nil {
		// The OS accepted every byte, but a Bluetooth serial stack may only have
		// buffered them. Claim `transmitted` only while the Bluetooth link is
		// confirmed up; otherwise the ticket's fate is unknowable.
		state, verr := w.linkState(ctx, activeCfg)
		if verr != nil || state != platform.LinkConnected {
			reason := fmt.Sprintf("bytes accepted but Bluetooth link state is %s", state)
			if verr != nil {
				reason += ": " + verr.Error()
			}
			w.closeTransport(StateDisconnected, reason)
			w.persist(ctx, func() error { return w.repo.MarkUncertain(run.UID, len(doc), "link_unknown_after_write", reason) })
			w.bus.Publish(events.Event{Type: events.PrinterDisconnected, PrinterID: w.cfg.ID,
				Message: "OS reports the printer disconnected"})
			return
		}
		now := time.Now().UTC()
		w.persist(ctx, func() error { return w.repo.MarkTransmitted(run.UID, len(doc)) })
		w.mu.Lock()
		w.lastTx = &now
		w.state = StateConnected
		w.mu.Unlock()
		return
	}

	// Any write error means the connection can no longer be trusted.
	bytesWritten := 0
	ambiguous := true
	if werr, ok := errors.AsType[*transport.WriteError](err); ok {
		bytesWritten = werr.BytesWritten
		ambiguous = werr.Ambiguous()
	}
	w.closeTransport(StateDisconnected, err.Error())
	w.bus.Publish(events.Event{Type: events.PrinterDisconnected, PrinterID: w.cfg.ID, Message: err.Error()})

	// A timed-out write may have partially reached the printer even when the
	// counted bytes are zero, so timeouts are always uncertain.
	if ambiguous {
		w.persist(ctx, func() error { return w.repo.MarkUncertain(run.UID, bytesWritten, "ambiguous_write", err.Error()) })
	} else {
		// Keep the failed row immutable. A new retry Run is created only after
		// this printer reconnects.
		w.persist(ctx, func() error { return w.repo.MarkFailed(run.UID, bytesWritten, "not_sent", err.Error(), true) })
	}
}

func (w *worker) persist(ctx context.Context, fn func() error) bool {
	err := fn()
	if err == nil {
		return true
	}
	if w.gate == nil {
		// Direct unit workers do not have a manager gate. Keep the production
		// invariant by retrying here as well.
		w.gate = newPersistenceGate()
	}
	w.gate.begin(err)
	if w.bus != nil {
		w.bus.Publish(events.Event{Type: events.PersistenceDegraded,
			Error: &events.Error{Code: "sqlite_unavailable", Message: "delivery-state persistence is unavailable", Retryable: true}})
	}
	delay := 250 * time.Millisecond
	for {
		w.mu.Lock()
		w.lastError = "database persistence paused: " + err.Error()
		w.mu.Unlock()
		if !waitContext(ctx, delay) {
			return false
		}
		nextErr := fn()
		if nextErr == nil {
			nextErr = w.repo.Ping()
		}
		if nextErr == nil {
			if w.gate.end() {
				w.bus.Publish(events.Event{Type: events.PersistenceRecovered})
			}
			w.mu.Lock()
			w.lastError = ""
			w.mu.Unlock()
			return true
		}
		err = nextErr
		w.gate.update(err)
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *worker) linkState(ctx context.Context, cfg config.PrinterConfig) (platform.LinkState, error) {
	if cfg.Transport == config.TransportMock {
		return platform.LinkConnected, nil
	}
	if verifier, ok := w.driver.(platform.LinkVerifier); ok {
		return verifier.LinkState(ctx, cfg)
	}
	// Compatibility for test/platform scaffolds without authoritative state.
	if err := w.driver.VerifyConnected(ctx, cfg); err != nil {
		if errors.Is(err, platform.ErrNotConnected) {
			return platform.LinkDisconnected, nil
		}
		return platform.LinkUnknown, err
	}
	return platform.LinkConnected, nil
}

func (w *worker) closeTransport(state ConnectionState, reason string) {
	w.mu.Lock()
	tr := w.tr
	activeCfg := w.activeCfg
	w.tr = nil
	w.activeCfg = config.PrinterConfig{}
	w.state = state
	if reason != "" {
		w.lastError = reason
	}
	w.mu.Unlock()
	if tr != nil {
		_ = tr.Close()
	}
	if activeCfg.Transport == config.TransportBluetoothSerial && w.driver != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = w.driver.Disconnect(ctx, activeCfg)
		cancel()
	}
}

func (w *worker) connectionConfig() config.PrinterConfig {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tr != nil {
		return w.activeCfg
	}
	return w.cfg
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
		Activity:         w.activity,
		Endpoint:         endpoint,
		LastError:        w.lastError,
		LastTransmission: w.lastTx,
		ReconnectAttempt: w.attempt,
		NextRetryAt:      w.nextRetry,
	}
}

func (w *worker) setActivity(activity ActivityState) {
	w.mu.Lock()
	w.activity = activity
	w.mu.Unlock()
}
