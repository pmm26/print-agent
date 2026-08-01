package printers

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/platform"
	"print-agent/internal/transport"
)

// TransportFactory builds a transport for a printer config. Production uses
// the platform driver's transport; tests inject mocks.
type TransportFactory func(config.PrinterConfig) transport.Transport

// driverTransportFactory routes mock printers to the in-process fake and
// everything else to the platform driver's transport.
func driverTransportFactory(driver platform.Driver) TransportFactory {
	return func(cfg config.PrinterConfig) transport.Transport {
		if cfg.Transport == config.TransportMock {
			return transport.NewMock(cfg.Endpoint)
		}
		return driver.NewTransport(cfg)
	}
}

// Manager owns all printer workers. It is the only component that starts,
// stops, or wakes them.
type Manager struct {
	configRepo *config.Repository
	jobsRepo   *jobs.Repository
	renderer   *escpos.Renderer
	driver     platform.Driver
	bus        *events.Bus
	factory    TransportFactory

	mu          sync.Mutex
	lifecycleMu sync.RWMutex
	ctx         context.Context
	started     bool
	workers     map[string]*worker
	gate        *persistenceGate
}

func NewManager(configRepo *config.Repository, jobsRepo *jobs.Repository,
	driver platform.Driver, bus *events.Bus, factory TransportFactory) *Manager {
	if factory == nil {
		factory = driverTransportFactory(driver)
	}
	return &Manager{
		configRepo: configRepo,
		jobsRepo:   jobsRepo,
		renderer:   escpos.NewRenderer(),
		driver:     driver,
		bus:        bus,
		factory:    factory,
		workers:    map[string]*worker{},
		gate:       newPersistenceGate(),
	}
}

// Start launches one worker per enabled printer. ctx cancellation stops all
// workers.
func (m *Manager) Start(ctx context.Context) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if ctx == nil {
		return fmt.Errorf("printer manager requires a context")
	}
	if m.started {
		return nil
	}
	printers, err := m.configRepo.ListPrinters()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.ctx = ctx
	m.started = true
	m.mu.Unlock()
	for _, p := range printers {
		if p.Enabled {
			m.startWorker(p)
		}
	}
	return nil
}

// Stop terminates all workers and waits for them to exit.
func (m *Manager) Stop() {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.workers = map[string]*worker{}
	m.ctx = nil
	m.started = false
	m.mu.Unlock()
	for _, w := range workers {
		w.stop()
	}
}

// BeginAcceptance prevents printer removal/configuration from racing with
// the validation and transaction of a print submission or reprint.
func (m *Manager) BeginAcceptance() func() {
	m.lifecycleMu.RLock()
	return m.lifecycleMu.RUnlock
}

func (m *Manager) startWorker(cfg config.PrinterConfig) {
	w := newWorker(cfg, m.jobsRepo, m.renderer, m.driver, m.bus, m.factory)
	w.gate = m.gate
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx == nil {
		return
	}
	w.start(ctx)
	m.mu.Lock()
	m.workers[cfg.ID] = w
	m.mu.Unlock()
}

func (m *Manager) stopWorker(id string) {
	m.mu.Lock()
	w := m.workers[id]
	delete(m.workers, id)
	m.mu.Unlock()
	if w != nil {
		w.stop()
	}
}

func (m *Manager) getWorker(id string) *worker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[id]
}

// Wake implements jobs.Waker.
func (m *Manager) Wake(printerID string) {
	if w := m.getWorker(printerID); w != nil {
		w.Wake()
	}
}

// PrinterExists implements jobs.PrinterDirectory. Disabled printers still
// accept jobs — they queue until the printer is enabled again.
func (m *Manager) PrinterExists(id string) bool {
	_, err := m.configRepo.GetPrinter(id)
	return err == nil
}

// PrinterConfig returns the active configuration used to snapshot a Job's
// rendering profile during acceptance.
func (m *Manager) PrinterConfig(id string) (config.PrinterConfig, error) {
	return m.configRepo.GetPrinter(id)
}

// ApplyPrinter persists a printer config and (re)starts its worker to pick
// up the change. The caller validates the config first.
func (m *Manager) ApplyPrinter(cfg config.PrinterConfig) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.applyPrinterLocked(cfg)
}

func (m *Manager) applyPrinterLocked(cfg config.PrinterConfig) error {
	processing, err := m.jobsRepo.HasProcessingRun(cfg.ID)
	if err != nil {
		return err
	}
	if processing {
		return jobs.NewConflictError("printer configuration cannot change while a Print Run is processing")
	}
	if err := m.configRepo.SavePrinter(cfg); err != nil {
		return err
	}
	m.stopWorker(cfg.ID)
	if cfg.Enabled {
		m.startWorker(cfg)
	}
	m.bus.Publish(events.Event{Type: events.ConfigChanged, PrinterID: cfg.ID,
		Message: "printer configuration saved"})
	return nil
}

func (m *Manager) CreatePrinter(cfg config.PrinterConfig) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if _, err := m.configRepo.GetPrinter(cfg.ID); err == nil {
		return jobs.NewConflictError("printer already exists; use PUT to update")
	} else if !errors.Is(err, config.ErrNotFound) {
		return err
	}
	return m.applyPrinterLocked(cfg)
}

func (m *Manager) UpdatePrinter(cfg config.PrinterConfig) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if _, err := m.configRepo.GetPrinter(cfg.ID); err != nil {
		return err
	}
	return m.applyPrinterLocked(cfg)
}

func (m *Manager) UpdatePrinterWith(id string, update func(config.PrinterConfig) (config.PrinterConfig, error)) (config.PrinterConfig, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	cfg, err := m.configRepo.GetPrinter(id)
	if err != nil {
		return cfg, err
	}
	cfg, err = update(cfg)
	if err != nil {
		return cfg, err
	}
	if err := m.applyPrinterLocked(cfg); err != nil {
		return cfg, err
	}
	return m.configRepo.GetPrinter(id)
}

// RemovePrinter stops the worker and retires the configuration. Historical
// Jobs continue to reference the stable printer ID.
func (m *Manager) RemovePrinter(id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	active, err := m.jobsRepo.HasActiveRuns(id)
	if err != nil {
		return err
	}
	if active {
		return jobs.NewConflictError("printer cannot be retired while queued, processing, or retry-pending Print Runs exist; disable it instead")
	}
	cfg, err := m.configRepo.GetPrinter(id)
	if err != nil {
		return err
	}
	m.stopWorker(id)
	if err := m.configRepo.DeletePrinter(id); err != nil {
		if cfg.Enabled {
			m.startWorker(cfg)
		}
		return err
	}
	m.bus.Publish(events.Event{Type: events.ConfigChanged, PrinterID: id, Message: "printer removed"})
	return nil
}

// SetEnabled enables or disables a printer and its worker.
func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	cfg, err := m.configRepo.GetPrinter(id)
	if err != nil {
		return err
	}
	cfg.Enabled = enabled
	return m.applyPrinterLocked(cfg)
}

// Reconnect forces an immediate reconnect attempt for one printer.
func (m *Manager) Reconnect(id string) error {
	w := m.getWorker(id)
	if w == nil {
		return fmt.Errorf("printer %s has no active worker (disabled or unknown)", id)
	}
	w.ReconnectNow()
	return nil
}

// ReconnectAll triggers reconnect on every active worker.
func (m *Manager) ReconnectAll() {
	m.mu.Lock()
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.mu.Unlock()
	for _, w := range workers {
		w.ReconnectNow()
	}
}

// Statuses returns a snapshot for every configured printer (including
// disabled ones, which have no worker), enriched with queue depths.
func (m *Manager) Statuses() ([]Status, error) {
	printers, err := m.configRepo.ListPrinters()
	if err != nil {
		return nil, err
	}
	depths, err := m.jobsRepo.QueueDepth()
	if err != nil {
		return nil, err
	}
	attention, err := m.jobsRepo.AttentionCount()
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(printers))
	for _, p := range printers {
		var st Status
		if w := m.getWorker(p.ID); w != nil {
			st = w.status()
			st.Printer = p
		} else {
			st = Status{Printer: p, State: StateDisabled, Endpoint: p.Endpoint}
			if p.Enabled {
				st.State = StateDisconnected
			}
		}
		st.QueueDepth = depths[p.ID]
		st.AttentionCount = attention[p.ID]
		out = append(out, st)
	}
	return out, nil
}

func (m *Manager) PersistenceStatus() persistenceStatus { return m.gate.status() }
