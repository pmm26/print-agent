package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"print-agent/internal/config"
	"print-agent/internal/events"
	"print-agent/internal/storage"
)

func TestOutboundClientRequiresAcknowledgementAndMarksOutboxAcked(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("PRINT_AGENT_TEST_WS_TOKEN", "top-secret")
	accepted := make(chan events.Event, 1)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer top-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := ws.Accept(w, r, &ws.AcceptOptions{Subprotocols: []string{Subprotocol}, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var frame struct {
			Type  string       `json:"type"`
			Event events.Event `json:"event"`
		}
		if json.Unmarshal(payload, &frame) != nil || frame.Type != "event" {
			return
		}
		accepted <- frame.Event
		_ = writeJSON(r.Context(), conn, time.Second, map[string]any{
			"type": "ack", "eventId": frame.Event.EventID, "sequence": frame.Event.Sequence,
		})
		<-r.Context().Done()
	}))
	defer remote.Close()
	destination := config.WebSocketDestination{ID: "primary", Enabled: true,
		Endpoint: strings.Replace(remote.URL, "http://", "ws://", 1), AuthType: "bearer",
		SecretRef: "env:PRINT_AGENT_TEST_WS_TOKEN", Categories: []string{"printer"},
		ConnectTimeout: time.Second, Heartbeat: time.Second, StaleTimeout: 2 * time.Second,
		WriteTimeout: time.Second, ReconnectMin: 100 * time.Millisecond, ReconnectMax: time.Second,
		AckTimeout: time.Second, OutboundQueueCapacity: 8}
	if err := config.NewRepository(db).SaveWebSocketDestination(destination); err != nil {
		t.Fatal(err)
	}
	store, err := events.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	want, err := store.Append(context.Background(), events.Event{Type: events.PrinterConnected, PrinterID: "kitchen"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(db, destination, events.NewDiscardPublisher())
	go client.Run(ctx)
	defer cancel()
	select {
	case got := <-accepted:
		if got.EventID != want.EventID || got.Sequence != want.Sequence {
			t.Fatalf("delivered = %+v, want %+v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("outbound event was not delivered")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		err := db.QueryRow(`SELECT status FROM event_deliveries WHERE destination_id = 'primary' AND event_sequence = ?`, want.Sequence).Scan(&status)
		if err == nil && status == "acked" {
			return
		}
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delivery was not acknowledged in SQLite")
}
