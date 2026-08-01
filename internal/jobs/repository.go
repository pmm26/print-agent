package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Repository persists jobs and deliveries. All timestamps are stored as
// RFC3339 UTC strings per the spec.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

var ErrNotFound = errors.New("not found")

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseTSPtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTS(s.String)
	return &t
}

// InsertJobWithDeliveries writes the job and all deliveries in one
// transaction. The caller must have checked for duplicates first; UNIQUE
// constraints are the final guard against races.
func (r *Repository) InsertJobWithDeliveries(job Job, deliveries []Delivery) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO print_jobs (id, external_job_id, source, created_at, accepted_at)
		VALUES (?, ?, ?, ?, ?)`,
		job.ID, job.ExternalJobID, job.Source, ts(job.CreatedAt), ts(job.AcceptedAt)); err != nil {
		return err
	}
	for _, d := range deliveries {
		if _, err := tx.Exec(`INSERT INTO print_deliveries
			(id, external_delivery_id, job_id, printer_id, template, payload_json, status, attempt_count, bytes_written, reprint_of, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
			d.ID, d.ExternalDeliveryID, d.JobID, d.PrinterID, d.Template,
			string(d.PayloadJSON), string(d.Status), nullable(d.ReprintOf), ts(d.CreatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InsertDelivery adds a single delivery to an existing job (reprints).
func (r *Repository) InsertDelivery(d Delivery) error {
	_, err := r.db.Exec(`INSERT INTO print_deliveries
		(id, external_delivery_id, job_id, printer_id, template, payload_json, status, attempt_count, bytes_written, reprint_of, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		d.ID, d.ExternalDeliveryID, d.JobID, d.PrinterID, d.Template,
		string(d.PayloadJSON), string(d.Status), nullable(d.ReprintOf), ts(d.CreatedAt))
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const deliveryCols = `id, external_delivery_id, job_id, printer_id, template, payload_json,
	status, attempt_count, bytes_written, COALESCE(last_error, ''), COALESCE(reprint_of, ''),
	created_at, started_at, transmitted_at, resolved_at`

func scanDelivery(row interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	var payload, created string
	var started, transmitted, resolved sql.NullString
	var status string
	err := row.Scan(&d.ID, &d.ExternalDeliveryID, &d.JobID, &d.PrinterID, &d.Template, &payload,
		&status, &d.AttemptCount, &d.BytesWritten, &d.LastError, &d.ReprintOf,
		&created, &started, &transmitted, &resolved)
	if err != nil {
		return d, err
	}
	d.PayloadJSON = json.RawMessage(payload)
	d.Status = DeliveryStatus(status)
	d.CreatedAt = parseTS(created)
	d.StartedAt = parseTSPtr(started)
	d.TransmittedAt = parseTSPtr(transmitted)
	d.ResolvedAt = parseTSPtr(resolved)
	return d, nil
}

// GetJobByExternalID loads a job and its deliveries, or ErrNotFound.
func (r *Repository) GetJobByExternalID(externalID string) (Job, []Delivery, error) {
	var j Job
	var created, accepted string
	err := r.db.QueryRow(`SELECT id, external_job_id, source, created_at, accepted_at
		FROM print_jobs WHERE external_job_id = ?`, externalID).
		Scan(&j.ID, &j.ExternalJobID, &j.Source, &created, &accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return j, nil, ErrNotFound
	}
	if err != nil {
		return j, nil, err
	}
	j.CreatedAt, j.AcceptedAt = parseTS(created), parseTS(accepted)
	deliveries, err := r.deliveriesWhere(`job_id = ? ORDER BY created_at`, j.ID)
	return j, deliveries, err
}

// GetDeliveryByExternalID loads one delivery, or ErrNotFound.
func (r *Repository) GetDeliveryByExternalID(externalID string) (Delivery, error) {
	row := r.db.QueryRow(`SELECT `+deliveryCols+` FROM print_deliveries WHERE external_delivery_id = ?`, externalID)
	d, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (r *Repository) deliveriesWhere(where string, args ...any) ([]Delivery, error) {
	rows, err := r.db.Query(`SELECT `+deliveryCols+` FROM print_deliveries WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDeliveries returns recent deliveries, optionally filtered.
func (r *Repository) ListDeliveries(printerID string, status DeliveryStatus, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := `1=1`
	args := []any{}
	if printerID != "" {
		where += ` AND printer_id = ?`
		args = append(args, printerID)
	}
	if status != "" {
		where += ` AND status = ?`
		args = append(args, string(status))
	}
	args = append(args, limit)
	return r.deliveriesWhere(where+` ORDER BY created_at DESC LIMIT ?`, args...)
}

// ClaimNextQueued atomically moves the oldest queued delivery for the
// printer to processing and returns it. Returns ErrNotFound when the queue
// is empty. The single UPDATE...RETURNING guarantees no two workers can
// claim the same delivery.
func (r *Repository) ClaimNextQueued(printerID string) (Delivery, error) {
	row := r.db.QueryRow(`UPDATE print_deliveries
		SET status = 'processing', started_at = ?, attempt_count = attempt_count + 1
		WHERE id = (SELECT id FROM print_deliveries
			WHERE printer_id = ? AND status = 'queued'
			ORDER BY created_at LIMIT 1)
		RETURNING `+deliveryCols, ts(time.Now()), printerID)
	d, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// MarkTransmitted finalizes a successful delivery.
func (r *Repository) MarkTransmitted(id string, bytesWritten int) error {
	return r.exec(`UPDATE print_deliveries
		SET status = 'transmitted', bytes_written = ?, transmitted_at = ?, last_error = NULL
		WHERE id = ?`, bytesWritten, ts(time.Now()), id)
}

// MarkFailed records a clean failure (nothing meaningful transmitted).
func (r *Repository) MarkFailed(id string, bytesWritten int, errText string) error {
	return r.exec(`UPDATE print_deliveries
		SET status = 'failed', bytes_written = ?, last_error = ?, resolved_at = NULL
		WHERE id = ?`, bytesWritten, errText, id)
}

// MarkUncertain records an ambiguous failure (transmission had begun).
func (r *Repository) MarkUncertain(id string, bytesWritten int, errText string) error {
	return r.exec(`UPDATE print_deliveries
		SET status = 'uncertain', bytes_written = ?, last_error = ?, resolved_at = NULL
		WHERE id = ?`, bytesWritten, errText, id)
}

// Requeue returns a claimed delivery to the queue (used when the write
// failed before any byte was sent and retry is safe).
func (r *Repository) Requeue(id string, errText string) error {
	return r.exec(`UPDATE print_deliveries
		SET status = 'queued', last_error = ?
		WHERE id = ?`, errText, id)
}

// Cancel cancels a delivery unless it is processing or already transmitted.
func (r *Repository) Cancel(id string) error {
	res, err := r.db.Exec(`UPDATE print_deliveries
		SET status = 'cancelled', resolved_at = ?
		WHERE id = ? AND status IN ('queued', 'failed', 'uncertain')`, ts(time.Now()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("delivery cannot be cancelled in its current state")
	}
	return nil
}

// Resolve marks a failed/uncertain delivery as acknowledged by the operator
// without reprinting. Status is preserved; resolved_at removes it from the
// attention list.
func (r *Repository) Resolve(id string) error {
	res, err := r.db.Exec(`UPDATE print_deliveries SET resolved_at = ?
		WHERE id = ? AND status IN ('failed', 'uncertain')`, ts(time.Now()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("only failed or uncertain deliveries can be resolved")
	}
	return nil
}

// CountReprints returns how many reprints exist for the given original.
func (r *Repository) CountReprints(originalExternalID string) (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM print_deliveries WHERE reprint_of = ?`,
		originalExternalID).Scan(&n)
	return n, err
}

// QueueDepth returns pending (queued + processing) deliveries per printer.
func (r *Repository) QueueDepth() (map[string]int, error) {
	rows, err := r.db.Query(`SELECT printer_id, COUNT(*) FROM print_deliveries
		WHERE status IN ('queued', 'processing') GROUP BY printer_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// AttentionCount returns unresolved failed/uncertain deliveries per printer.
func (r *Repository) AttentionCount() (map[string]int, error) {
	rows, err := r.db.Query(`SELECT printer_id, COUNT(*) FROM print_deliveries
		WHERE status IN ('failed', 'uncertain') AND resolved_at IS NULL GROUP BY printer_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// RecoverAbandoned flips deliveries left in processing (service stopped
// mid-write) to uncertain, per the crash-recovery rules. Returns the
// affected external delivery IDs.
func (r *Repository) RecoverAbandoned() ([]string, error) {
	rows, err := r.db.Query(`UPDATE print_deliveries
		SET status = 'uncertain', last_error = 'agent restarted while processing'
		WHERE status = 'processing'
		RETURNING external_delivery_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PurgeOlderThan deletes terminal deliveries, their jobs, and events older
// than the retention window.
func (r *Repository) PurgeOlderThan(cutoff time.Time) error {
	c := ts(cutoff)
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM print_deliveries
		WHERE created_at < ? AND status IN ('transmitted', 'cancelled', 'failed')`, c); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM print_jobs WHERE created_at < ?
		AND NOT EXISTS (SELECT 1 FROM print_deliveries d WHERE d.job_id = print_jobs.id)`, c); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM print_events WHERE created_at < ?`, c); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) exec(query string, args ...any) error {
	_, err := r.db.Exec(query, args...)
	return err
}
