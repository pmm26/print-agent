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
	"sync"
	"time"

	"print-agent/internal/api"
	"print-agent/internal/config"
	"print-agent/internal/diagnostics"
	"print-agent/internal/escpos"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
	appLogging "print-agent/internal/logging"
	"print-agent/internal/platform"
	"print-agent/internal/platform/host"
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
	// Driver overrides the platform driver (tests).
	Driver platform.Driver
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
	opts      Options
	log       *slog.Logger
	logStore  *appLogging.Store
	logFile   *appLogging.RollingFile
	db        *sql.DB
	bus       *events.Bus
	manager   *printers.Manager
	server    *http.Server
	closeOnce sync.Once
	closeErr  error
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
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(opts.DataDir, 0o700); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(opts.DataDir, "print-agent.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	logStore := appLogging.NewStore(db)
	logger, logFile, err := buildLogger(opts, logStore)
	if err != nil {
		db.Close()
		return nil, err
	}

	bus := events.NewBus()
	diag := diagnostics.NewService(db, dbPath)
	bus.Subscribe(func(e events.Event) {
		if err := diag.PersistEvent(e.Type, e.PrinterID, e.RunUID, e.Message, e.CreatedAt); err != nil {
			logger.Error("printer event persistence failed", "type", e.Type, "printerId", e.PrinterID,
				"runUid", e.RunUID, "error", err)
		}
	})

	configRepo := config.NewRepository(db)
	jobsRepo := jobs.NewRepository(db)

	// Crash recovery: Print Runs stuck in processing become uncertain.
	recovered, err := jobsRepo.RecoverAbandoned()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("crash recovery: %w", err)
	}
	for _, run := range recovered {
		bus.Publish(events.Event{Type: events.PrintRunUncertain, RunUID: run.UID, PrinterID: run.PrinterID,
			Message: "recovered as uncertain after restart"})
	}

	driver := opts.Driver
	if driver == nil {
		driver = host.New()
	}
	manager := printers.NewManager(configRepo, jobsRepo, driver, bus, opts.TransportFactory)
	jobsService := jobs.NewService(jobsRepo, bus, manager, escpos.NewRenderer(), escpos.KnownTemplate)
	jobsService.SetWaker(manager)
	jobsService.SetPayloadValidator(escpos.ValidateTemplateData)

	auth := api.NewAuthService(db)
	server := api.NewServer(jobsService, jobsRepo, manager, configRepo, driver, diag, logStore, auth, bus, logger)

	return &Service{
		opts:     opts,
		log:      logger,
		logStore: logStore,
		logFile:  logFile,
		db:       db,
		bus:      bus,
		manager:  manager,
		server: &http.Server{
			Handler:           server.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 * 1024,
		},
	}, nil
}

func buildLogger(opts Options, store *appLogging.Store) (*slog.Logger, *appLogging.RollingFile, error) {
	logDir := filepath.Join(opts.DataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(logDir, 0o700); err != nil {
		return nil, nil, err
	}
	logPath := filepath.Join(logDir, "print-agent.log")
	fileSink, err := appLogging.NewRollingFile(logPath)
	if err != nil {
		return nil, nil, err
	}
	var sink io.Writer = fileSink
	if opts.Console {
		sink = io.MultiWriter(os.Stderr, fileSink)
	}
	jsonHandler := slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(appLogging.NewHandler(jsonHandler, store)), fileSink, nil
}

// Run starts workers and the HTTP listener, then blocks until ctx is
// cancelled or the listener fails.
func (s *Service) Run(ctx context.Context) error {
	defer s.Close()
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
	s.log.Info("agent started", "address", addr, "version", diagnostics.Version,
		"dashboard", fmt.Sprintf("http://%s/admin", addr))

	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		s.retentionLoop(workerCtx)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.server.Serve(ln) }()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP server stopped unexpectedly", "error", err)
			runErr = err
		}
	}

	s.bus.Publish(events.Event{Type: events.AgentStopping})
	s.log.Info("agent stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.server.Shutdown(shutdownCtx)
	stopWorkers()
	s.manager.Stop()
	<-retentionDone
	return runErr
}

// Close releases resources for services that never started as well as
// normally-running services. It is safe to call more than once.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		_ = s.server.Close()
		s.manager.Stop()
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// retentionLoop enforces the short system-log window every minute while
// retaining the existing daily Job, Print Run, and printer-event cleanup.
func (s *Service) retentionLoop(ctx context.Context) {
	repo := jobs.NewRepository(s.db)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var lastJobPurge time.Time
	for {
		now := time.Now().UTC()
		if err := s.logStore.Purge(now); err != nil {
			s.log.Error("system log retention purge failed", "error", err)
		}
		if err := s.logFile.Purge(now); err != nil {
			s.log.Error("system log file retention purge failed", "error", err)
		}
		if lastJobPurge.IsZero() || now.Sub(lastJobPurge) >= 24*time.Hour {
			if err := repo.PurgeExpired(now); err != nil {
				s.log.Error("retention purge failed", "error", err)
			} else if err := storage.IncrementalVacuum(s.db); err != nil {
				s.log.Error("incremental vacuum failed", "error", err)
			}
			lastJobPurge = now
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
