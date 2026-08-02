// Package websocket implements replayable inbound event streaming and the
// acknowledged outbound event outbox. It never accepts remote commands.
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ws "github.com/coder/websocket"

	"print-agent/internal/config"
	"print-agent/internal/events"
)

const Subprotocol = "print-agent.events.v1"

type SnapshotFunc func(context.Context) (any, error)
type AuthorizeFunc func(*http.Request) bool

type Server struct {
	settings  config.WebSocketSettings
	store     *events.Store
	publisher events.Publisher
	authorize AuthorizeFunc
	snapshot  SnapshotFunc
	clients   atomic.Int64
	dropped   atomic.Uint64
	closing   chan struct{}
	closeOnce sync.Once
}

func NewServer(settings config.WebSocketSettings, store *events.Store, publisher events.Publisher,
	authorize AuthorizeFunc, snapshot SnapshotFunc) *Server {
	return &Server{settings: settings, store: store, publisher: publisher, authorize: authorize,
		snapshot: snapshot, closing: make(chan struct{})}
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s.authorize == nil || !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.reserveClient() {
		http.Error(w, "WebSocket connection limit reached", http.StatusServiceUnavailable)
		return
	}
	reserved := true
	defer func() {
		if reserved {
			s.clients.Add(-1)
		}
	}()
	select {
	case <-s.closing:
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	default:
	}
	conn, err := ws.Accept(w, r, &ws.AcceptOptions{
		Subprotocols:       []string{Subprotocol},
		InsecureSkipVerify: true, // Origin and credentials were checked above.
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(s.settings.MaxMessageBytes)
	reserved = false
	defer s.clients.Add(-1)
	defer conn.CloseNow()
	s.publishLocal(events.WebSocketServerConnected)
	defer s.publishLocal(events.WebSocketServerDisconnected)

	cursor, err := parseCursor(r)
	if err != nil {
		_ = conn.Close(ws.StatusPolicyViolation, err.Error())
		return
	}
	categories, err := parseCategories(r)
	if err != nil {
		_ = conn.Close(ws.StatusPolicyViolation, err.Error())
		return
	}
	oldest, latest, err := s.store.Bounds(r.Context(), true)
	if err != nil {
		_ = conn.Close(ws.StatusInternalError, "event store unavailable")
		return
	}
	if cursor > 0 && oldest > 0 && cursor < oldest-1 {
		_ = writeJSON(r.Context(), conn, s.settings.WriteTimeout, map[string]any{
			"type": "resync_required", "oldestAvailableSequence": oldest, "latestSequence": latest,
		})
		_ = conn.Close(ws.StatusPolicyViolation, "event cursor expired")
		return
	}
	if cursor > latest {
		_ = writeJSON(r.Context(), conn, s.settings.WriteTimeout, map[string]any{
			"type": "resync_required", "oldestAvailableSequence": oldest, "latestSequence": latest,
		})
		_ = conn.Close(ws.StatusPolicyViolation, "event cursor is ahead of this agent")
		return
	}
	if cursor == 0 && s.snapshot != nil {
		value, err := s.snapshot(r.Context())
		if err != nil {
			_ = conn.Close(ws.StatusInternalError, "snapshot unavailable")
			return
		}
		if err := writeJSON(r.Context(), conn, s.settings.WriteTimeout, map[string]any{
			"type": "snapshot", "schemaVersion": events.SchemaVersion,
			"latestSequence": latest, "snapshot": value,
		}); err != nil {
			return
		}
		cursor = latest
	}

	queue := make(chan events.Event, s.settings.ClientQueueCapacity)
	producerCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	producerErr := make(chan error, 1)
	go s.produce(producerCtx, cursor, categories, queue, producerErr)
	inputErr := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(producerCtx)
		if err == nil {
			err = errors.New("inbound WebSocket commands are not supported")
		}
		inputErr <- err
	}()
	heartbeat := time.NewTicker(s.settings.Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-s.closing:
			_ = conn.Close(ws.StatusGoingAway, "agent stopping")
			return
		case err := <-producerErr:
			if errors.Is(err, errSlowConsumer) {
				s.dropped.Add(1)
				if s.publisher != nil {
					s.publisher.Publish(events.Event{Type: events.WebSocketEventDropped,
						PublishScope: events.ScopeLocal, Message: "slow server consumer evicted"})
				}
				_ = conn.Close(ws.StatusPolicyViolation, "event consumer is too slow")
			}
			return
		case err := <-inputErr:
			if err != nil && producerCtx.Err() == nil {
				_ = conn.Close(ws.StatusUnsupportedData, "inbound commands are not supported")
			}
			return
		case event := <-queue:
			if err := writeJSON(r.Context(), conn, s.settings.WriteTimeout,
				map[string]any{"type": "event", "event": event}); err != nil {
				return
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(r.Context(), s.settings.WriteTimeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) reserveClient() bool {
	limit := int64(s.settings.ConnectionLimit)
	for {
		current := s.clients.Load()
		if current >= limit {
			return false
		}
		if s.clients.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

var errSlowConsumer = errors.New("slow WebSocket consumer")

func (s *Server) produce(ctx context.Context, cursor int64, categories map[string]bool,
	queue chan<- events.Event, result chan<- error) {
	for {
		batch, err := s.store.After(ctx, cursor, s.settings.ReplayLimit, true)
		if err != nil {
			result <- err
			return
		}
		for _, event := range batch {
			cursor = event.Sequence
			if len(categories) > 0 && !categories[events.CategoryFor(event.Type)] {
				continue
			}
			select {
			case queue <- event:
			default:
				result <- errSlowConsumer
				return
			}
		}
		if len(batch) == s.settings.ReplayLimit {
			continue
		}
		select {
		case <-ctx.Done():
			result <- ctx.Err()
			return
		case <-s.closing:
			result <- context.Canceled
			return
		case <-s.store.Wake():
		case <-time.After(time.Second):
		}
	}
}

func parseCategories(r *http.Request) (map[string]bool, error) {
	raw := r.URL.Query().Get("categories")
	if raw == "" {
		return nil, nil
	}
	allowed := map[string]bool{"agent": true, "printer": true, "job": true, "print_run": true, "config": true}
	result := map[string]bool{}
	for _, category := range strings.Split(raw, ",") {
		category = strings.TrimSpace(category)
		if !allowed[category] {
			return nil, errors.New("unsupported event category")
		}
		result[category] = true
	}
	if len(result) == 0 {
		return nil, errors.New("at least one event category is required")
	}
	return result, nil
}

func parseCursor(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("cursor must be a non-negative event sequence")
	}
	return cursor, nil
}

func writeJSON(parent context.Context, conn *ws.Conn, timeout time.Duration, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return conn.Write(ctx, ws.MessageText, payload)
}

func (s *Server) publishLocal(eventType string) {
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: eventType, PublishScope: events.ScopeLocal})
	}
}

func (s *Server) Close() { s.closeOnce.Do(func() { close(s.closing) }) }

func (s *Server) Status() map[string]any {
	return map[string]any{"connectedClients": s.clients.Load(), "droppedEvents": s.dropped.Load()}
}
