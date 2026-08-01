package printers

import (
	"context"
	"fmt"
	"sync"

	"print-agent/internal/bluetooth"
	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/transport"
)

// TransportFactory builds a transport for a printer config. Production uses
// DefaultTransportFactory; tests inject mocks.
type TransportFactory func(config.PrinterConfig) transport.Transport

// DefaultTransportFactory maps the configured transport kind to a real
// implementation.
func DefaultTransportFactory(cfg config.PrinterConfig) transport.Transport {
	if cfg.Transport == config.TransportMock {
		return transport.NewMock(cfg.Endpoint)
	}
	return transport.NewSerial(cfg)
}

// Manager owns all printer workers. It is the only component that starts,
// stops, or wakes them.
type Manager struct {
	configRepo *config.Repository
	jobsRepo   *jobs.Repository
	renderer   *escpos.Renderer
	connector  bluetooth.Connector
	bus        *events.Bus
	factory    TransportFactory

	mu      sync.Mutex
	ctx     context.Context
	workers map[string]*worker
}

func NewManager(configRepo *config.Repository, jobsRepo *jobs.Repository,
	connector bluetooth.Connector, bus *events.Bus, factory TransportFactory) *Manager {
	if factory == nil {
		factory = DefaultTransportFactory
	}
	return &Manager{
		configRepo: configRepo,
		jobsRepo:   jobsRepo,
		renderer:   escpos.NewRenderer(),
		connector:  connector,
		bus:        bus,
		factory:    factory,
		workers:    map[string]*worker{},
	}
}

// Start launches one worker per enabled printer. ctx cancellation stops all
// workers.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	printers, err := m.configRepo.ListPrinters()
	if err != nil {
		return err
	}
	for _, p := range printers {
		if p.Enabled {
			m.startWorker(p)
		}
	}
	return nil
}

// Stop terminates all workers and waits for them to exit.
func (m *Manager) Stop() {
	m.mu.Lock()
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.workers = map[string]*worker{}
	m.mu.Unlock()
	for _, w := range workers {
		w.stop()
	}
}

func (m *Manager) startWorker(cfg config.PrinterConfig) {
	w := newWorker(cfg, m.jobsRepo, m.renderer, m.connector, m.bus, m.factory)
	m.mu.Lock()
	m.workers[cfg.ID] = w
	ctx := m.ctx
	m.mu.Unlock()
	w.start(ctx)
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

// ApplyPrinter persists a printer config and (re)starts its worker to pick
// up the change. The caller validates the config first.
func (m *Manager) ApplyPrinter(cfg config.PrinterConfig) error {
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

// RemovePrinter stops the worker and deletes the configuration. Historical
// deliveries are preserved.
func (m *Manager) RemovePrinter(id string) error {
	if err := m.configRepo.DeletePrinter(id); err != nil {
		return err
	}
	m.stopWorker(id)
	m.bus.Publish(events.Event{Type: events.ConfigChanged, PrinterID: id, Message: "printer removed"})
	return nil
}

// SetEnabled enables or disables a printer and its worker.
func (m *Manager) SetEnabled(id string, enabled bool) error {
	cfg, err := m.configRepo.GetPrinter(id)
	if err != nil {
		return err
	}
	cfg.Enabled = enabled
	return m.ApplyPrinter(cfg)
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
