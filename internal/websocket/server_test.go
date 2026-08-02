package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"print-agent/internal/config"
	"print-agent/internal/events"
	"print-agent/internal/storage"
)

func testEventStore(t *testing.T) (*sql.DB, *events.Store, func()) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := events.NewStore(db)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db, store, func() { db.Close() }
}

func testServerSettings() config.WebSocketSettings {
	return config.WebSocketSettings{Mode: config.WebSocketServer, ServerPath: "/api/v2/events/ws",
		ServerBindAddress: "127.0.0.1", ServerAuthRequired: true, ClientQueueCapacity: 8,
		ConnectionLimit: 4, Heartbeat: time.Second, WriteTimeout: time.Second,
		MaxMessageBytes: 16 * 1024, ReplayLimit: 100}
}

func dialTestServer(t *testing.T, url string) *ws.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := ws.Dial(ctx, strings.Replace(url, "http://", "ws://", 1),
		&ws.DialOptions{Subprotocols: []string{Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func readFrame(t *testing.T, conn *ws.Conn) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestServerSnapshotLiveDeliveryAndReplay(t *testing.T) {
	_, store, closeStore := testEventStore(t)
	defer closeStore()
	server := NewServer(testServerSettings(), store, events.NewDiscardPublisher(), func(*http.Request) bool { return true },
		func(context.Context) (any, error) { return map[string]any{"ready": true}, nil })
	if _, err := store.Append(context.Background(), events.Event{Type: events.AgentReady}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()

	conn := dialTestServer(t, httpServer.URL)
	defer conn.CloseNow()
	frame := readFrame(t, conn)
	if string(frame["type"]) != `"snapshot"` {
		t.Fatalf("first frame = %s", frame["type"])
	}
	event, err := store.Append(context.Background(), events.Event{Type: events.PrinterConnecting,
		PrinterID: "kitchen", Metadata: map[string]any{"connectionAttempt": 1}})
	if err != nil {
		t.Fatal(err)
	}
	frame = readFrame(t, conn)
	var delivered events.Event
	if err := json.Unmarshal(frame["event"], &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.EventID != event.EventID || delivered.Sequence == 0 || delivered.PrinterID != "kitchen" {
		t.Fatalf("delivered event = %+v", delivered)
	}

	replay := dialTestServer(t, httpServer.URL+"?cursor="+strconv.FormatInt(event.Sequence-1, 10))
	defer replay.CloseNow()
	frame = readFrame(t, replay)
	if string(frame["type"]) != `"event"` {
		t.Fatalf("replay frame = %s", frame["type"])
	}
}

func TestServerRejectsUnauthorizedBeforeUpgrade(t *testing.T) {
	_, store, closeStore := testEventStore(t)
	defer closeStore()
	server := NewServer(testServerSettings(), store, events.NewDiscardPublisher(), func(*http.Request) bool { return false }, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, response, err := ws.Dial(ctx, strings.Replace(httpServer.URL, "http://", "ws://", 1), nil)
	if err == nil || response == nil || response.StatusCode != 401 {
		t.Fatalf("dial error = %v, response = %#v", err, response)
	}
}

func TestServerSignalsExpiredCursor(t *testing.T) {
	db, store, closeStore := testEventStore(t)
	defer closeStore()
	var eventsAdded []events.Event
	for i := 0; i < 3; i++ {
		event, err := store.Append(context.Background(), events.Event{Type: events.AgentReady})
		if err != nil {
			t.Fatal(err)
		}
		eventsAdded = append(eventsAdded, event)
	}
	if _, err := db.Exec(`DELETE FROM durable_events WHERE sequence < ?`, eventsAdded[2].Sequence); err != nil {
		t.Fatal(err)
	}
	server := NewServer(testServerSettings(), store, events.NewDiscardPublisher(), func(*http.Request) bool { return true }, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()
	conn := dialTestServer(t, httpServer.URL+"?cursor="+strconv.FormatInt(eventsAdded[0].Sequence, 10))
	defer conn.CloseNow()
	frame := readFrame(t, conn)
	if string(frame["type"]) != `"resync_required"` {
		t.Fatalf("frame = %#v", frame)
	}
}

func TestServerSignalsCursorAheadOfAgent(t *testing.T) {
	_, store, closeStore := testEventStore(t)
	defer closeStore()
	latest, err := store.Append(context.Background(), events.Event{Type: events.AgentReady})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(testServerSettings(), store, events.NewDiscardPublisher(), func(*http.Request) bool { return true }, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()
	conn := dialTestServer(t, httpServer.URL+"?cursor="+strconv.FormatInt(latest.Sequence+10, 10))
	defer conn.CloseNow()
	frame := readFrame(t, conn)
	if string(frame["type"]) != `"resync_required"` {
		t.Fatalf("frame = %#v", frame)
	}
}

func TestServerCategoryFilterAdvancesAcrossSkippedEvents(t *testing.T) {
	_, store, closeStore := testEventStore(t)
	defer closeStore()
	seed, _ := store.Append(context.Background(), events.Event{Type: events.AgentReady})
	_, _ = store.Append(context.Background(), events.Event{Type: events.JobAccepted, JobUID: "job_1"})
	want, _ := store.Append(context.Background(), events.Event{Type: events.PrinterConnected, PrinterID: "kitchen"})
	server := NewServer(testServerSettings(), store, events.NewDiscardPublisher(), func(*http.Request) bool { return true }, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()
	url := httpServer.URL + "?cursor=" + strconv.FormatInt(seed.Sequence, 10) + "&categories=printer"
	conn := dialTestServer(t, url)
	defer conn.CloseNow()
	frame := readFrame(t, conn)
	var delivered events.Event
	if err := json.Unmarshal(frame["event"], &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.EventID != want.EventID {
		t.Fatalf("filtered event = %+v", delivered)
	}
}
