package events

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const defaultRetention = 7 * 24 * time.Hour

type Store struct {
	db        *sql.DB
	agentID   string
	retention time.Duration
	wake      chan struct{}
}

type Stats struct {
	OldestSequence     int64      `json:"oldestSequence"`
	LatestSequence     int64      `json:"latestSequence"`
	EventCount         int64      `json:"eventCount"`
	PendingDeliveries  int64      `json:"pendingDeliveries"`
	OldestPendingAt    *time.Time `json:"oldestPendingAt,omitempty"`
	DeadLetterCount    int64      `json:"deadLetterCount"`
	DatabaseBytes      int64      `json:"databaseBytes"`
	DiskHighWaterBytes int64      `json:"diskHighWaterBytes"`
	DiskPressure       bool       `json:"diskPressure"`
}

func NewStore(db *sql.DB) (*Store, error) {
	store := &Store{db: db, retention: defaultRetention, wake: make(chan struct{}, 1)}
	if err := store.ensureSettings(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) ensureSettings() error {
	var retentionSeconds int64
	err := s.db.QueryRow(`SELECT agent_id, event_retention_seconds FROM agent_settings WHERE singleton = 1`).Scan(&s.agentID, &retentionSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		s.agentID = "agent_" + hex.EncodeToString(b)
		now := timestamp(time.Now())
		if _, err := s.db.Exec(`INSERT INTO agent_settings
			(singleton, agent_id, created_at, updated_at) VALUES (1, ?, ?, ?)`, s.agentID, now, now); err != nil {
			return err
		}
		retentionSeconds = int64(defaultRetention.Seconds())
	} else if err != nil {
		return err
	}
	s.retention = time.Duration(retentionSeconds) * time.Second
	return nil
}

func (s *Store) AgentID() string       { return s.agentID }
func (s *Store) Wake() <-chan struct{} { return s.wake }

func (s *Store) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Store) Append(ctx context.Context, event Event) (Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return event, err
	}
	defer tx.Rollback()
	event, err = s.AppendTx(tx, event)
	if err != nil {
		return event, err
	}
	if err := tx.Commit(); err != nil {
		return event, err
	}
	s.notify()
	return event, nil
}

func (s *Store) AppendTx(tx *sql.Tx, event Event) (Event, error) {
	now := time.Now().UTC()
	if err := event.Prepare(s.agentID, now, s.retention); err != nil {
		return event, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return event, err
	}
	var errorCode string
	if event.Error != nil {
		errorCode = event.Error.Code
	}
	result, err := tx.Exec(`INSERT INTO durable_events
		(event_id, schema_version, event_type, category, occurred_at, recorded_at, agent_id,
		 publish_scope, durability, printer_id, job_uid, external_job_id, template, run_uid,
		 correlation_id, causation_event_id, error_code, safe_message, payload_json, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.EventID, event.SchemaVersion, event.Type, event.Category,
		timestamp(event.OccurredAt), timestamp(now), event.AgentID, string(event.PublishScope),
		string(event.Durability), nullable(event.PrinterID), nullable(event.JobUID), nullable(event.JobID),
		nullable(event.Template), nullable(event.RunUID), nullable(event.CorrelationID),
		nullable(event.CausationEventID), nullable(errorCode), nullable(event.Message), string(payload),
		timestamp(event.ExpiresAt))
	if err != nil {
		return event, err
	}
	event.Sequence, err = result.LastInsertId()
	if err != nil {
		return event, err
	}
	payload, err = json.Marshal(event)
	if err != nil {
		return event, err
	}
	if _, err := tx.Exec(`UPDATE durable_events SET payload_json = ? WHERE sequence = ?`, string(payload), event.Sequence); err != nil {
		return event, err
	}
	if event.PublishScope == ScopeExternal {
		_, err = tx.Exec(`INSERT INTO event_deliveries (destination_id, event_sequence, next_attempt_at)
			SELECT d.id, ?, ? FROM websocket_destinations d
			WHERE d.enabled = 1 AND EXISTS (
				SELECT 1 FROM json_each(d.categories_json) WHERE value = ?
			) AND (SELECT COUNT(*) FROM event_deliveries pending
				WHERE pending.destination_id = d.id AND pending.status IN ('pending','inflight')) < d.outbound_queue_capacity`,
			event.Sequence, timestamp(now), event.Category)
		if err != nil {
			return event, err
		}
		_, err = tx.Exec(`INSERT INTO dead_letter_events
			(destination_id, event_id, event_sequence, event_type, payload_json, failed_at, reason,
			 error_code, error_message, attempt_count, expires_at)
			SELECT d.id, ?, ?, ?, ?, ?, 'outbound_capacity_exhausted', 'buffer_full',
			'outbound durable buffer capacity was reached', 0, ? FROM websocket_destinations d
			WHERE d.enabled = 1 AND EXISTS (
				SELECT 1 FROM json_each(d.categories_json) WHERE value = ?
			) AND NOT EXISTS (SELECT 1 FROM event_deliveries delivery
				WHERE delivery.destination_id = d.id AND delivery.event_sequence = ?)`,
			event.EventID, event.Sequence, event.Type, string(payload), timestamp(now),
			timestamp(now.Add(30*24*time.Hour)), event.Category, event.Sequence)
		if err != nil {
			return event, err
		}
	}
	return event, nil
}

func (s *Store) NotifyCommit() { s.notify() }

func (s *Store) After(ctx context.Context, after int64, limit int, externalOnly bool) ([]Event, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := `SELECT payload_json FROM durable_events WHERE sequence > ?`
	if externalOnly {
		query += ` AND publish_scope = 'external'`
	}
	query += ` ORDER BY sequence LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Event
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event Event
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, fmt.Errorf("decode durable event: %w", err)
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) Bounds(ctx context.Context, externalOnly bool) (oldest, latest int64, err error) {
	where := ""
	if externalOnly {
		where = ` WHERE publish_scope = 'external'`
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(sequence), 0), COALESCE(MAX(sequence), 0) FROM durable_events`+where).Scan(&oldest, &latest)
	return
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var result Stats
	var oldestPending sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MIN(sequence) FROM durable_events), 0),
		COALESCE((SELECT MAX(sequence) FROM durable_events), 0),
		(SELECT COUNT(*) FROM durable_events),
		(SELECT COUNT(*) FROM event_deliveries WHERE status IN ('pending','inflight')),
		(SELECT MIN(e.recorded_at) FROM event_deliveries d JOIN durable_events e ON e.sequence=d.event_sequence
			WHERE d.status IN ('pending','inflight')),
		(SELECT COUNT(*) FROM dead_letter_events)`).Scan(&result.OldestSequence, &result.LatestSequence,
		&result.EventCount, &result.PendingDeliveries, &oldestPending, &result.DeadLetterCount)
	if err != nil {
		return result, err
	}
	if oldestPending.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, oldestPending.String)
		if err != nil {
			return result, err
		}
		result.OldestPendingAt = &parsed
	}
	var pageCount, pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT event_disk_high_water_bytes FROM agent_settings WHERE singleton = 1`).Scan(&result.DiskHighWaterBytes); err != nil {
		return result, err
	}
	result.DatabaseBytes = pageCount * pageSize
	result.DiskPressure = result.DatabaseBytes >= result.DiskHighWaterBytes
	return result, nil
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
