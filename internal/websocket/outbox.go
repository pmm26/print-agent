package websocket

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"print-agent/internal/events"
)

const maxDeliveryAttempts = 10

type delivery struct {
	DestinationID string
	Event         events.Event
	Attempt       int
	AttemptRowID  int64
}

type outbox struct{ db *sql.DB }

func (o outbox) recoverInflight(destinationID string) error {
	_, err := o.db.Exec(`UPDATE event_deliveries SET status = 'pending', acknowledgement_deadline = NULL,
		next_attempt_at = ? WHERE destination_id = ? AND status = 'inflight'`, stamp(time.Now()), destinationID)
	return err
}

func (o outbox) claim(destinationID string, ackTimeout time.Duration) (delivery, error) {
	tx, err := o.db.Begin()
	if err != nil {
		return delivery{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var d delivery
	var payload string
	err = tx.QueryRow(`UPDATE event_deliveries SET status = 'inflight', attempt_count = attempt_count + 1,
		first_attempt_at = COALESCE(first_attempt_at, ?), last_attempt_at = ?, acknowledgement_deadline = ?
		WHERE (destination_id, event_sequence) = (
			SELECT destination_id, event_sequence FROM event_deliveries
			WHERE destination_id = ? AND status = 'pending' AND next_attempt_at <= ?
			ORDER BY event_sequence LIMIT 1
		) RETURNING destination_id, event_sequence, attempt_count`,
		stamp(now), stamp(now), stamp(now.Add(ackTimeout)), destinationID, stamp(now)).Scan(
		&d.DestinationID, &d.Event.Sequence, &d.Attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, sql.ErrNoRows
	}
	if err != nil {
		return d, err
	}
	if err := tx.QueryRow(`SELECT payload_json FROM durable_events WHERE sequence = ?`, d.Event.Sequence).Scan(&payload); err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(payload), &d.Event); err != nil {
		return d, fmt.Errorf("decode outbox event: %w", err)
	}
	result, err := tx.Exec(`INSERT INTO outbox_attempts
		(destination_id, event_sequence, attempt_number, started_at) VALUES (?, ?, ?, ?)`,
		d.DestinationID, d.Event.Sequence, d.Attempt, stamp(now))
	if err != nil {
		return d, err
	}
	d.AttemptRowID, err = result.LastInsertId()
	if err != nil {
		return d, err
	}
	if err := tx.Commit(); err != nil {
		return d, err
	}
	return d, nil
}

func (o outbox) ack(d delivery) error {
	now := stamp(time.Now())
	tx, err := o.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE event_deliveries SET status = 'acked', acknowledged_at = ?,
		acknowledgement_deadline = NULL, last_error_code = NULL, last_error_message = NULL
		WHERE destination_id = ? AND event_sequence = ? AND status = 'inflight'`,
		now, d.DestinationID, d.Event.Sequence)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("delivery state changed before acknowledgement")
	}
	if _, err := tx.Exec(`UPDATE outbox_attempts SET finished_at = ?, outcome = 'acked' WHERE id = ?`, now, d.AttemptRowID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE websocket_destinations SET last_delivery_at = ?, last_ack_at = ?,
		consecutive_failures = 0, last_error_code = NULL, last_error_message = NULL, updated_at = ? WHERE id = ?`,
		now, now, now, d.DestinationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (o outbox) retry(d delivery, delay time.Duration, code, message string) error {
	now := time.Now().UTC()
	if d.Attempt >= maxDeliveryAttempts {
		return o.deadLetter(d, code, message)
	}
	tx, err := o.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE event_deliveries SET status = 'pending', next_attempt_at = ?,
		acknowledgement_deadline = NULL, last_error_code = ?, last_error_message = ?
		WHERE destination_id = ? AND event_sequence = ?`, stamp(now.Add(delay)), code, message,
		d.DestinationID, d.Event.Sequence); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE outbox_attempts SET finished_at = ?, outcome = 'retry',
		error_code = ?, error_message = ? WHERE id = ?`, stamp(now), code, message, d.AttemptRowID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE websocket_destinations SET consecutive_failures = consecutive_failures + 1,
		last_error_code = ?, last_error_message = ?, updated_at = ? WHERE id = ?`,
		code, message, stamp(now), d.DestinationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (o outbox) deadLetter(d delivery, code, message string) error {
	now := time.Now().UTC()
	payload, err := json.Marshal(d.Event)
	if err != nil {
		return err
	}
	tx, err := o.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE event_deliveries SET status = 'dead_letter', acknowledgement_deadline = NULL,
		last_error_code = ?, last_error_message = ? WHERE destination_id = ? AND event_sequence = ?`,
		code, message, d.DestinationID, d.Event.Sequence); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO dead_letter_events
		(destination_id, event_id, event_sequence, event_type, payload_json, failed_at, reason,
		 error_code, error_message, attempt_count, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.DestinationID, d.Event.EventID, d.Event.Sequence, d.Event.Type, string(payload), stamp(now),
		"delivery_attempts_exhausted", code, message, d.Attempt, stamp(now.Add(30*24*time.Hour))); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE outbox_attempts SET finished_at = ?, outcome = 'dead_letter',
		error_code = ?, error_message = ? WHERE id = ?`, stamp(now), code, message, d.AttemptRowID); err != nil {
		return err
	}
	return tx.Commit()
}

func stamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
