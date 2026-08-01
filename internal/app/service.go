// Package app wires all components together and owns the process lifecycle:
// startup crash recovery, worker management, the HTTP listener, and
// graceful shutdown.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"print-agent/internal/api"
	"print-agent/internal/bluetooth"
	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	"print-agent/internal/printers"
	"print-agent/internal/storage"
)

// Options configure one agent instance.
type Options struct {
	DataDir string
	Port    int
	Console bool // also log human-readably to stderr
	// TransportFactory overrides the default (integration tests).
	TransportFactory printers.TransportFactory
	// Connector overrides the platform Bluetooth connector (tests).
	Connector bluetooth.Connector
	// RetentionDays prunes terminal deliveries/events older than this.
	RetentionDays int
}

// DefaultDataDir returns the per-OS data directory.
func DefaultDataDir() string {
	switch {
	case os.Getenv("PRINT_AGENT_DATA") != "":
		return os.Getenv("PRINT_AGENT_DATA")
	default:
		base, err := os.UserConfigDir() // macOS: ~/Library/Application Support
		if err != nil {
			base = "."
		}
		return filepath.Join(base, "print-agent")
	}
}

// Service is a fully wired agent.
type Service struct {
	opts    Options
	log     *slog.Logger
	db      *sql.DB
	bus     *events.Bus
	manager *printers.Manager
	server  *http.Server
}

// New builds the agent: opens the database, applies migrations, performs
// crash recovery, and wires every component. Call Run to start it.
func New(opts Options) (*Service, error) {
	if opts.Port == 0 {
		opts.Port = 17432
	}
	if opts.DataDir == "" {
		opts.DataDir = DefaultDataDir()
	}
	if opts.RetentionDays == 0 {
		opts.RetentionDays = 30
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, err
	}

	logger, err := buildLogger(opts)
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(opts.DataDir, "print-agent.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	bus := events.NewBus()
	diag := diagnostics.NewService(db, dbPath)
	bus.Subscribe(func(e events.Event) {
		diag.PersistEvent(e.Type, e.PrinterID, e.DeliveryID, e.Message, e.CreatedAt)
		logger.Info(e.Type, "printer", e.PrinterID, "delivery", e.DeliveryID, "message", e.Message)
	})

	configRepo := config.NewRepository(db)
	jobsRepo := jobs.NewRepository(db)

	// Crash recovery: deliveries stuck in processing become uncertain.
	recovered, err := jobsRepo.RecoverAbandoned()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("crash recovery: %w", err)
	}
	for _, id := range recovered {
		bus.Publish(events.Event{Type: events.DeliveryUncertain, DeliveryID: id,
			Message: "recovered as uncertain after restart"})
	}

	connector := opts.Connector
	if connector == nil {
		connector = bluetooth.NewPlatformConnector()
	}
	manager := printers.NewManager(configRepo, jobsRepo, connector, bus, opts.TransportFactory)
	jobsService := jobs.NewService(jobsRepo, bus, manager, escpos.KnownTemplate)
	jobsService.SetWaker(manager)

	auth := api.NewAuthService(db)
	server := api.NewServer(jobsService, jobsRepo, manager, configRepo, connector, diag, auth, bus, logger)

	return &Service{
		opts: opts,
		log:  logger,
		db:   db,
		bus:  bus,
		manager: manager,
		server: &http.Server{
			Handler:           server.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		},
	}, nil
}

func buildLogger(opts Options) (*slog.Logger, error) {
	logDir := filepath.Join(opts.DataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	fileSink := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "print-agent.log"),
		MaxSize:    10, // MB
		MaxBackups: 5,
		MaxAge:     30, // days
	}
	var sink io.Writer = fileSink
	if opts.Console {
		sink = io.MultiWriter(fileSink, os.Stderr)
	}
	return slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), nil
}

// Run starts workers and the HTTP listener, then blocks until ctx is
// cancelled or the listener fails.
func (s *Service) Run(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.opts.Port)
	ln, err := net.Listen("tcp", addr) // loopback only, by design
	if err != nil {
		return fmt.Errorf("listen on %s (is another agent running?): %w", addr, err)
	}

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	if err := s.manager.Start(workerCtx); err != nil {
		ln.Close()
		return err
	}
	s.bus.Publish(events.Event{Type: events.AgentStarted,
		Message: fmt.Sprintf("listening on http://%s (version %s)", addr, diagnostics.Version)})
	if s.opts.Console {
		fmt.Printf("print-agent running — dashboard: http://%s/admin\n", addr)
	}

	go s.retentionLoop(workerCtx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.server.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	s.bus.Publish(events.Event{Type: events.AgentStopping})
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.server.Shutdown(shutdownCtx)
	s.manager.Stop()
	stopWorkers()
	return s.db.Close()
}

// retentionLoop purges old terminal data daily.
func (s *Service) retentionLoop(ctx context.Context) {
	repo := jobs.NewRepository(s.db)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		cutoff := time.Now().AddDate(0, 0, -s.opts.RetentionDays)
		if err := repo.PurgeOlderThan(cutoff); err != nil {
			s.log.Error("retention purge failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
