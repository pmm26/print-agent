package events

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"print-agent/internal/storage"
)

func testStore(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, store
}

func TestAppendAllocatesStablePerAgentSequence(t *testing.T) {
	_, store := testStore(t)
	first, err := store.Append(context.Background(), Event{Type: AgentReady})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Append(context.Background(), Event{Type: PrinterConnecting, PrinterID: "kitchen"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence == 0 || second.Sequence != first.Sequence+1 || first.AgentID != second.AgentID {
		t.Fatalf("sequences = %d, %d; agents = %q, %q", first.Sequence, second.Sequence, first.AgentID, second.AgentID)
	}
	replayed, err := store.After(context.Background(), first.Sequence, 10, true)
	if err != nil || len(replayed) != 1 || replayed[0].EventID != second.EventID {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
}

func TestAppendTxRollsBackWithBusinessTransaction(t *testing.T) {
	db, store := testStore(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendTx(tx, Event{Type: PrintRunTransmitted, RunUID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	_, latest, err := store.Bounds(context.Background(), false)
	if err != nil || latest != 0 {
		t.Fatalf("latest after rollback = %d, %v", latest, err)
	}
}

func TestExternalEventCreatesOnlyMatchingDestinationDelivery(t *testing.T) {
	db, store := testStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range []struct{ id, categories string }{{"printer", `["printer"]`}, {"jobs", `["job"]`}} {
		if _, err := db.Exec(`INSERT INTO websocket_destinations
			(id, enabled, endpoint, auth_type, categories_json, created_at, updated_at)
			VALUES (?, 0, 'wss://events.example.test', 'none', ?, ?, ?)`, row.id, row.categories, now, now); err != nil {
			t.Fatal(err)
		}
	}
	// The schema intentionally allows only one enabled destination.
	if _, err := db.Exec(`UPDATE websocket_destinations SET enabled = 1 WHERE id = 'printer'`); err != nil {
		t.Fatal(err)
	}
	event, err := store.Append(context.Background(), Event{Type: PrinterConnected, PrinterID: "kitchen"})
	if err != nil {
		t.Fatal(err)
	}
	var destination string
	if err := db.QueryRow(`SELECT destination_id FROM event_deliveries WHERE event_sequence = ?`, event.Sequence).Scan(&destination); err != nil {
		t.Fatal(err)
	}
	if destination != "printer" {
		t.Fatalf("destination = %q", destination)
	}
	local, err := store.Append(context.Background(), Event{Type: WebSocketDeliveryFailed, PublishScope: ScopeLocal})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM event_deliveries WHERE event_sequence = ?`, local.Sequence).Scan(&count); err != nil || count != 0 {
		t.Fatalf("local delivery count = %d, %v", count, err)
	}
}

func TestSensitiveFieldsAreRejected(t *testing.T) {
	_, store := testStore(t)
	if _, err := store.Append(context.Background(), Event{Type: AgentError,
		Metadata: map[string]any{"authorization": "Bearer secret"}}); err == nil {
		t.Fatal("sensitive event was accepted")
	}
}

func TestOutboundCapacityCreatesExplicitDeadLetter(t *testing.T) {
	db, store := testStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO websocket_destinations
		(id, enabled, endpoint, auth_type, categories_json, outbound_queue_capacity, created_at, updated_at)
		VALUES ('limited', 1, 'wss://events.example.test', 'none', '["printer"]', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), Event{Type: PrinterConnected, PrinterID: "one"}); err != nil {
		t.Fatal(err)
	}
	second, err := store.Append(context.Background(), Event{Type: PrinterConnected, PrinterID: "two"})
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := db.QueryRow(`SELECT reason FROM dead_letter_events WHERE event_id = ?`, second.EventID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "outbound_capacity_exhausted" {
		t.Fatalf("dead-letter reason = %q", reason)
	}
}

func TestRecorderBackpressureNeverBlocksPublisher(t *testing.T) {
	_, store := testStore(t)
	recorder := NewRecorder(store, 1)
	if !recorder.Publish(Event{Type: PrinterConnecting}) {
		t.Fatal("first operational event was unexpectedly dropped")
	}
	started := time.Now()
	if recorder.Publish(Event{Type: PrinterConnecting}) {
		t.Fatal("full bounded recorder queue accepted another event")
	}
	if time.Since(started) > 50*time.Millisecond || recorder.Dropped() != 1 {
		t.Fatalf("bounded publish blocked or failed to count drop: %v, %d", time.Since(started), recorder.Dropped())
	}
}
