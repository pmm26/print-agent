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
	"runtime"
	"strings"
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
	appWebSocket "print-agent/internal/websocket"
)

// Options configure one agent instance.
type Options struct {
	DataDir     string
	Port        int
	BindAddress string
	Console     bool // also log human-readably to stderr
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
	opts        Options
	log         *slog.Logger
	logStore    *appLogging.Store
	logFile     *appLogging.RollingFile
	db          *sql.DB
	eventStore  *events.Store
	recorder    *events.Recorder
	manager     *printers.Manager
	server      *http.Server
	wsServer    *appWebSocket.Server
	wsClient    *appWebSocket.Client
	bindAddress string
	tlsCertPath string
	tlsKeyPath  string
	closeOnce   sync.Once
	closeErr    error
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

	eventStore, err := events.NewStore(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize event store: %w", err)
	}
	recorder := events.NewRecorder(eventStore, 2048)
	diag := diagnostics.NewService(db, dbPath)

	configRepo := config.NewRepository(db)
	jobsRepo := jobs.NewRepository(db, eventStore)

	// Crash recovery distinguishes claimed (known not sent) from transmitting
	// (ambiguous and therefore uncertain) attempts.
	recovered, err := jobsRepo.RecoverAbandoned()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("crash recovery: %w", err)
	}
	_ = recovered // repository persisted precise recovery events atomically

	driver := opts.Driver
	if driver == nil {
		driver = host.New()
	}
	manager := printers.NewManager(configRepo, jobsRepo, driver, recorder, opts.TransportFactory)
	jobsService := jobs.NewService(jobsRepo, recorder, manager, escpos.NewRenderer(), escpos.KnownTemplate)
	jobsService.SetWaker(manager)
	jobsService.SetPayloadValidator(escpos.ValidateTemplateData)

	auth := api.NewAuthService(db)
	server := api.NewServer(jobsService, jobsRepo, manager, configRepo, driver, diag, logStore, auth, recorder, logger)
	wsSettings, err := configRepo.WebSocketSettings()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("load WebSocket settings: %w", err)
	}
	if opts.BindAddress == "" {
		opts.BindAddress = wsSettings.ServerBindAddress
	}
	wsSettings.ServerBindAddress = opts.BindAddress
	if err := wsSettings.Validate(opts.BindAddress); err != nil {
		db.Close()
		return nil, fmt.Errorf("validate WebSocket settings: %w", err)
	}
	if wsSettings.ServerTLS && runtime.GOOS != "windows" {
		keyPath := strings.TrimPrefix(wsSettings.TLSKeyRef, "file:")
		info, err := os.Stat(keyPath)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("validate TLS private key: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			db.Close()
			return nil, fmt.Errorf("TLS private key must not be accessible by group or other users")
		}
	}
	var wsServer *appWebSocket.Server
	if wsSettings.Mode == config.WebSocketServer || wsSettings.Mode == config.WebSocketBoth {
		authorize := func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin != "" && !api.IsLocalOrigin(origin) {
				if !configRepo.WebSocketOriginAllowed(origin) {
					return false
				}
			}
			if !wsSettings.ServerAuthRequired {
				return true
			}
			return auth.AuthorizeEventsRequest(r)
		}
		snapshot := func(context.Context) (any, error) {
			statuses, err := manager.Statuses()
			if err != nil {
				return nil, err
			}
			return map[string]any{"agentId": eventStore.AgentID(), "printers": statuses,
				"persistence": manager.PersistenceStatus()}, nil
		}
		wsServer = appWebSocket.NewServer(wsSettings, eventStore, recorder, authorize, snapshot)
		server.SetEventHandler(wsSettings.ServerPath, wsServer.Handler())
	}
	var wsClient *appWebSocket.Client
	if wsSettings.Mode == config.WebSocketClient || wsSettings.Mode == config.WebSocketBoth {
		destination, err := configRepo.EnabledWebSocketDestination()
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("load outbound WebSocket destination: %w", err)
		}
		if err := destination.Validate(); err != nil {
			db.Close()
			return nil, fmt.Errorf("validate outbound WebSocket destination: %w", err)
		}
		wsClient = appWebSocket.NewClient(db, destination, recorder)
	}
	server.SetRuntimeStatus(func(ctx context.Context) map[string]any {
		stats, err := eventStore.Stats(ctx)
		result := map[string]any{"droppedOperationalEvents": recorder.Dropped()}
		if err != nil {
			result["error"] = "event status unavailable"
		} else {
			result["oldestSequence"] = stats.OldestSequence
			result["latestSequence"] = stats.LatestSequence
			result["eventCount"] = stats.EventCount
			result["pendingDeliveries"] = stats.PendingDeliveries
			result["oldestPendingAt"] = stats.OldestPendingAt
			result["deadLetterCount"] = stats.DeadLetterCount
			result["databaseBytes"] = stats.DatabaseBytes
			result["diskHighWaterBytes"] = stats.DiskHighWaterBytes
			result["diskPressure"] = stats.DiskPressure
		}
		if wsServer != nil {
			result["server"] = wsServer.Status()
		}
		if wsClient != nil {
			result["outbound"] = wsClient.Status()
		}
		return result
	})

	return &Service{
		opts:        opts,
		log:         logger,
		logStore:    logStore,
		logFile:     logFile,
		db:          db,
		eventStore:  eventStore,
		recorder:    recorder,
		wsServer:    wsServer,
		wsClient:    wsClient,
		bindAddress: opts.BindAddress,
		tlsCertPath: wsSettings.TLSCertPath,
		tlsKeyPath:  strings.TrimPrefix(wsSettings.TLSKeyRef, "file:"),
		manager:     manager,
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
	s.recorder.Start(ctx)
	_, _ = s.eventStore.Append(context.Background(), events.Event{Type: events.AgentStarting})
	addr := net.JoinHostPort(s.bindAddress, fmt.Sprintf("%d", s.opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.recordAgentError("listener_start_failed", "HTTP listener could not start")
		return fmt.Errorf("listen on %s (is another agent running?): %w", addr, err)
	}

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	if err := s.manager.Start(workerCtx); err != nil {
		ln.Close()
		s.recordAgentError("printer_manager_start_failed", "printer manager could not start")
		return err
	}
	if s.wsClient != nil {
		go s.wsClient.Run(workerCtx)
	}
	scheme := "http"
	if s.tlsCertPath != "" {
		scheme = "https"
	}
	_, _ = s.eventStore.Append(context.Background(), events.Event{Type: events.AgentReady,
		Message: fmt.Sprintf("listening on %s://%s (version %s)", scheme, addr, diagnostics.Version)})
	s.log.Info("agent started", "address", addr, "version", diagnostics.Version,
		"dashboard", fmt.Sprintf("%s://%s/admin", scheme, addr))

	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		s.retentionLoop(workerCtx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		if s.tlsCertPath != "" {
			serveErr <- s.server.ServeTLS(ln, s.tlsCertPath, s.tlsKeyPath)
			return
		}
		serveErr <- s.server.Serve(ln)
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP server stopped unexpectedly", "error", err)
			s.recordAgentError("http_server_failed", "HTTP server stopped unexpectedly")
			runErr = err
		}
	}

	_, _ = s.eventStore.Append(context.Background(), events.Event{Type: events.AgentStopping})
	s.log.Info("agent stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.wsServer != nil {
		s.wsServer.Close()
	}
	s.server.Shutdown(shutdownCtx)
	stopWorkers()
	s.manager.Stop()
	<-retentionDone
	_ = s.recorder.Stop(shutdownCtx)
	return runErr
}

func (s *Service) recordAgentError(code, message string) {
	_, _ = s.eventStore.Append(context.Background(), events.Event{Type: events.AgentError,
		Error: &events.Error{Code: code, Message: message}})
}

// Close releases resources for services that never started as well as
// normally-running services. It is safe to call more than once.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		_ = s.server.Close()
		if s.wsServer != nil {
			s.wsServer.Close()
		}
		s.manager.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.recorder.Stop(shutdownCtx)
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
