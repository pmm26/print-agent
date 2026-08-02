package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"print-agent/internal/events"
)

const retentionWindow = 48 * time.Hour

type Repository struct {
	db     *sql.DB
	events *events.Store
}

func NewRepository(db *sql.DB, stores ...*events.Store) *Repository {
	var store *events.Store
	if len(stores) > 0 {
		store = stores[0]
	} else {
		store, _ = events.NewStore(db)
	}
	return &Repository{db: db, events: store}
}

var (
	ErrNotFound         = errors.New("not found")
	ErrStateChanged     = errors.New("print run state changed concurrently")
	ErrDuplicateRequest = errors.New("request identifier already exists")
	ErrTargetFulfilled  = errors.New("original printer target is already fulfilled")
)

type DeduplicationRecord struct {
	Hash      string
	CreatedAt time.Time
}

type RetryRenderFunc func(Job, JobPrinter, PrintRun, int) (string, error)
type ReprintRenderFunc func(Job, JobPrinter, int) (string, error)

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s timestamp %q: %w", field, value, err)
	}
	return t, nil
}

func parseTSPtr(field string, value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	t, err := parseTS(field, value.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const jobCols = `uid, job_id, template, data_json, request_hash, source, owner_origin, created_at, expires_at`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var data, created, expires string
	if err := row.Scan(&j.UID, &j.JobID, &j.Template, &data, &j.RequestHash, &j.Source,
		&j.OwnerOrigin, &created, &expires); err != nil {
		return j, err
	}
	j.Data = json.RawMessage(data)
	var err error
	if j.CreatedAt, err = parseTS("created_at", created); err != nil {
		return j, err
	}
	if j.ExpiresAt, err = parseTS("expires_at", expires); err != nil {
		return j, err
	}
	return j, nil
}

const targetCols = `job_uid, printer_id, target_order, encoding, characters_per_line,
	accepted_content_hash, cancelled_at, COALESCE(cancellation_reason, '')`

func scanTarget(row interface{ Scan(...any) error }) (JobPrinter, error) {
	var p JobPrinter
	var cancelled sql.NullString
	if err := row.Scan(&p.JobUID, &p.PrinterID, &p.TargetOrder, &p.Encoding,
		&p.CharactersPerLine, &p.AcceptedContentHash, &cancelled, &p.CancellationReason); err != nil {
		return p, err
	}
	var err error
	if p.CancelledAt, err = parseTSPtr("cancelled_at", cancelled); err != nil {
		return p, err
	}
	return p, nil
}

const runCols = `queue_sequence, uid, job_uid, printer_id, chain_uid, run_number, attempt_number, trigger, content_mode,
	COALESCE(previous_run_uid, ''), COALESCE(reprint_of_run_uid, ''),
	COALESCE(reprint_request_id, ''), COALESCE(reprint_reason, ''), expected_content_hash,
	status, retry_disposition, COALESCE(resolution, ''), COALESCE(error_code, ''),
	COALESCE(error_message, ''), bytes_accepted, created_at, claimed_at, transmission_started_at,
	finished_at, transmitted_at, resolved_at`

func scanRun(row interface{ Scan(...any) error }) (PrintRun, error) {
	var run PrintRun
	var trigger, mode, status, retryDisposition, resolution, created string
	var claimed, transmissionStarted, finished, transmitted, resolved sql.NullString
	if err := row.Scan(&run.QueueSequence, &run.UID, &run.JobUID, &run.PrinterID,
		&run.ChainUID, &run.RunNumber, &run.AttemptNumber, &trigger, &mode, &run.PreviousRunUID, &run.ReprintOfRunUID,
		&run.ReprintRequestID, &run.ReprintReason, &run.ExpectedContentHash, &status,
		&retryDisposition, &resolution, &run.ErrorCode, &run.ErrorMessage, &run.BytesAccepted,
		&created, &claimed, &transmissionStarted, &finished, &transmitted, &resolved); err != nil {
		return run, err
	}
	run.Trigger = PrintRunTrigger(trigger)
	run.ContentMode = ContentMode(mode)
	run.Status = PrintRunStatus(status)
	run.RetryDisposition = RetryDisposition(retryDisposition)
	run.Retryable = run.RetryDisposition == RetryPendingReconnect
	run.Resolution = RunResolution(resolution)
	var err error
	if run.CreatedAt, err = parseTS("created_at", created); err != nil {
		return run, err
	}
	if run.ClaimedAt, err = parseTSPtr("claimed_at", claimed); err != nil {
		return run, err
	}
	if run.TransmissionStartedAt, err = parseTSPtr("transmission_started_at", transmissionStarted); err != nil {
		return run, err
	}
	if run.FinishedAt, err = parseTSPtr("finished_at", finished); err != nil {
		return run, err
	}
	if run.TransmittedAt, err = parseTSPtr("transmitted_at", transmitted); err != nil {
		return run, err
	}
	if run.ResolvedAt, err = parseTSPtr("resolved_at", resolved); err != nil {
		return run, err
	}
	return run, nil
}

func (r *Repository) InsertJob(job Job, targets []JobPrinter, runs []PrintRun) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO jobs
		(uid, job_id, template, data_json, request_hash, source, owner_origin, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.UID, job.JobID, job.Template,
		string(job.Data), job.RequestHash, job.Source, job.OwnerOrigin, ts(job.CreatedAt), ts(job.ExpiresAt)); err != nil {
		return err
	}
	for _, p := range targets {
		if _, err := tx.Exec(`INSERT INTO job_targets
			(job_uid, printer_id, target_order, encoding, characters_per_line, accepted_content_hash)
			VALUES (?, ?, ?, ?, ?, ?)`, p.JobUID, p.PrinterID, p.TargetOrder, p.Encoding,
			p.CharactersPerLine, p.AcceptedContentHash); err != nil {
			return err
		}
	}
	for _, run := range runs {
		if err := insertRunTx(tx, run); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO idempotency_records
		(kind, scope_uid, request_key, request_hash, resource_uid, created_at, expires_at)
		VALUES ('job_acceptance', 'agent', ?, ?, ?, ?, ?)`, job.JobID+"\x00"+job.Template,
		job.RequestHash, job.UID, ts(job.CreatedAt), ts(job.ExpiresAt)); err != nil {
		return err
	}
	if r.events != nil {
		if _, err := r.events.AppendTx(tx, events.Event{Type: events.JobAccepted, JobUID: job.UID,
			JobID: job.JobID, Template: job.Template, CorrelationID: job.UID,
			Metadata: map[string]any{"targetCount": len(targets), "currentState": "accepted"}}); err != nil {
			return err
		}
		for _, run := range runs {
			if _, err := r.events.AppendTx(tx, runEvent(events.PrintRunQueued, job, run, map[string]any{"currentState": "queued"})); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if r.events != nil {
		r.events.NotifyCommit()
	}
	return nil
}

func insertRunTx(tx *sql.Tx, run PrintRun) error {
	if run.ChainUID == "" {
		run.ChainUID = run.UID
	}
	if run.AttemptNumber == 0 {
		run.AttemptNumber = 1
	}
	if run.RetryDisposition == "" {
		run.RetryDisposition = RetryNone
	}
	_, err := tx.Exec(`INSERT INTO print_runs
		(uid, job_uid, printer_id, chain_uid, run_number, attempt_number, trigger, content_mode, previous_run_uid,
		 reprint_of_run_uid, reprint_request_id, reprint_reason, expected_content_hash,
		 status, retry_disposition, resolution, error_code, error_message, bytes_accepted,
		 created_at, claimed_at, transmission_started_at, finished_at, transmitted_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.UID, run.JobUID, run.PrinterID, run.ChainUID, run.RunNumber, run.AttemptNumber, string(run.Trigger), string(run.ContentMode),
		nullable(run.PreviousRunUID), nullable(run.ReprintOfRunUID), nullable(run.ReprintRequestID),
		nullable(run.ReprintReason), run.ExpectedContentHash, string(run.Status), string(run.RetryDisposition),
		nullable(string(run.Resolution)), nullable(run.ErrorCode), nullable(run.ErrorMessage),
		run.BytesAccepted, ts(run.CreatedAt), timeOrNil(run.ClaimedAt), timeOrNil(run.TransmissionStartedAt),
		timeOrNil(run.FinishedAt), timeOrNil(run.TransmittedAt), timeOrNil(run.ResolvedAt))
	return err
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func (r *Repository) GetJob(uid string) (Job, error) {
	j, err := scanJob(r.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE uid = ?`, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

func (r *Repository) GetJobByIdentity(jobID, template string) (Job, error) {
	j, err := scanJob(r.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE job_id = ? AND template = ?`, jobID, template))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

func (r *Repository) targetsFor(jobUID string) ([]JobPrinter, error) {
	rows, err := r.db.Query(`SELECT `+targetCols+` FROM job_targets WHERE job_uid = ? ORDER BY target_order`, jobUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobPrinter
	for rows.Next() {
		p, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repository) GetTarget(jobUID, printerID string) (JobPrinter, error) {
	target, err := scanTarget(r.db.QueryRow(`SELECT `+targetCols+` FROM job_targets
		WHERE job_uid = ? AND printer_id = ?`, jobUID, printerID))
	if errors.Is(err, sql.ErrNoRows) {
		return target, ErrNotFound
	}
	return target, err
}

func (r *Repository) runsWhere(where string, args ...any) ([]PrintRun, error) {
	rows, err := r.db.Query(`SELECT `+runCols+` FROM print_runs WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrintRun
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r *Repository) GetDetail(uid string) (JobDetail, error) {
	job, err := r.GetJob(uid)
	if err != nil {
		return JobDetail{}, err
	}
	targets, err := r.targetsFor(uid)
	if err != nil {
		return JobDetail{}, err
	}
	runs, err := r.runsWhere(`job_uid = ? ORDER BY printer_id, run_number`, uid)
	if err != nil {
		return JobDetail{}, err
	}
	return DeriveJob(job, targets, runs), nil
}

func (r *Repository) ListJobDetails(jobID, template, attention, owner string, limit int, before *time.Time, beforeUID string) ([]JobDetail, error) {
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	query := `SELECT ` + jobCols + ` FROM jobs WHERE 1=1`
	args := []any{}
	if jobID != "" {
		query += ` AND job_id = ?`
		args = append(args, jobID)
	}
	if template != "" {
		query += ` AND template = ?`
		args = append(args, template)
	}
	if owner != "" && owner != "local" {
		query += ` AND owner_origin = ?`
		args = append(args, owner)
	}
	if attention != "" {
		predicate := `EXISTS (SELECT 1 FROM print_runs ar WHERE ar.job_uid = jobs.uid
			AND ar.resolution IS NULL
			AND (ar.status = 'uncertain' OR (ar.status = 'failed' AND ar.retry_disposition IN ('none','exhausted','suppressed')))
			AND (ar.content_mode = 'reprint' OR (
				NOT EXISTS (SELECT 1 FROM print_runs ok WHERE ok.job_uid = ar.job_uid
					AND ok.printer_id = ar.printer_id AND
					(ok.status = 'transmitted' OR (ok.status = 'uncertain' AND ok.resolution = 'confirmed_printed')))
				AND NOT EXISTS (SELECT 1 FROM job_targets cp WHERE cp.job_uid = ar.job_uid
					AND cp.printer_id = ar.printer_id AND cp.cancelled_at IS NOT NULL)
			)))`
		if attention == "true" {
			query += ` AND ` + predicate
		} else {
			query += ` AND NOT ` + predicate
		}
	}
	if before != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND uid < ?))`
		args = append(args, ts(*before), ts(*before), beforeUID)
	}
	query += ` ORDER BY created_at DESC, uid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []JobDetail
	for _, job := range jobs {
		detail, err := r.GetDetail(job.UID)
		if err != nil {
			return nil, err
		}
		out = append(out, detail)
	}
	return out, nil
}

func (r *Repository) GetRun(uid string) (PrintRun, error) {
	run, err := scanRun(r.db.QueryRow(`SELECT `+runCols+` FROM print_runs WHERE uid = ?`, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return run, ErrNotFound
	}
	return run, err
}

func (r *Repository) ClaimNextQueued(printerID string) (PrintRun, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return PrintRun{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRow(`UPDATE print_runs
		SET status = 'claimed', claimed_at = ?
		WHERE queue_sequence = (
			SELECT queue_sequence FROM print_runs
			WHERE printer_id = ? AND status = 'queued'
			ORDER BY queue_sequence LIMIT 1
		)
		RETURNING `+runCols, ts(time.Now()), printerID))
	if errors.Is(err, sql.ErrNoRows) {
		return run, ErrNotFound
	}
	if err != nil {
		return run, err
	}
	if err := r.appendRunEventTx(tx, events.PrintRunClaimed, run, map[string]any{
		"previousState": "queued", "currentState": "claimed",
	}); err != nil {
		return run, err
	}
	if err := tx.Commit(); err != nil {
		return run, err
	}
	if r.events != nil {
		r.events.NotifyCommit()
	}
	return run, nil
}

func (r *Repository) transition(uid string, target PrintRunStatus, eventType, query string, metadata map[string]any, args ...any) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	changed := n == 1
	if !changed {
		var current string
		if err := tx.QueryRow(`SELECT status FROM print_runs WHERE uid = ?`, uid).Scan(&current); err != nil || PrintRunStatus(current) != target {
			return ErrStateChanged
		}
	}
	if _, err := tx.Exec(`UPDATE jobs SET expires_at = ? WHERE uid = (SELECT job_uid FROM print_runs WHERE uid = ?)`,
		ts(time.Now().UTC().Add(retentionWindow)), uid); err != nil {
		return err
	}
	run, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM print_runs WHERE uid = ?`, uid))
	if err != nil {
		return err
	}
	if changed {
		if metadata == nil {
			metadata = map[string]any{}
		}
		if _, exists := metadata["previousState"]; !exists {
			switch target {
			case RunFailed:
				if run.TransmissionStartedAt != nil {
					metadata["previousState"] = string(RunTransmitting)
				} else {
					metadata["previousState"] = string(RunClaimed)
				}
			case RunCancelled:
				metadata["previousState"] = string(RunQueued)
			}
		}
		metadata["currentState"] = string(target)
		if err := r.appendRunEventTx(tx, eventType, run, metadata); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if r.events != nil {
		r.events.NotifyCommit()
	}
	return nil
}

func (r *Repository) MarkTransmitted(uid string, bytesAccepted int) error {
	now := time.Now().UTC()
	return r.transition(uid, RunTransmitted, events.PrintRunTransmitted, `UPDATE print_runs SET status = 'transmitted', retry_disposition = 'none',
		bytes_accepted = ?, transmitted_at = ?, finished_at = ?, error_code = NULL, error_message = NULL
		WHERE uid = ? AND status = 'transmitting'`, map[string]any{
		"previousState": "transmitting", "bytesAccepted": bytesAccepted,
		"linkVerifiedAfterWrite": true, "physicalPrintConfirmed": false,
	}, bytesAccepted, ts(now), ts(now), uid)
}

func (r *Repository) MarkFailed(uid string, bytesAccepted int, code, message string, retryable bool) error {
	disposition := RetryNone
	if retryable {
		disposition = RetryPendingReconnect
	}
	return r.transition(uid, RunFailed, events.PrintRunFailed, `UPDATE print_runs SET status = 'failed', retry_disposition = ?,
		bytes_accepted = ?, error_code = ?, error_message = ?, finished_at = ?
		WHERE uid = ? AND status IN ('claimed','transmitting')`, map[string]any{
		"bytesAccepted": bytesAccepted, "retryDisposition": string(disposition),
		"physicalPrintConfirmed": false,
	}, string(disposition), bytesAccepted, code, message, ts(time.Now()), uid)
}

func (r *Repository) MarkUncertain(uid string, bytesAccepted int, code, message string) error {
	return r.transition(uid, RunUncertain, events.PrintRunUncertain, `UPDATE print_runs SET status = 'uncertain', retry_disposition = 'none',
		bytes_accepted = ?, error_code = ?, error_message = ?, finished_at = ?
		WHERE uid = ? AND status = 'transmitting'`, map[string]any{
		"previousState": "transmitting", "knownBytesAccepted": bytesAccepted,
		"automaticRetryAllowed": false, "physicalPrintConfirmed": false,
		"operatorAttentionRequired": true,
	}, bytesAccepted, code, message, ts(time.Now()), uid)
}

func (r *Repository) MarkTransmitting(uid string) error {
	return r.transition(uid, RunTransmitting, events.PrintRunTransmissionStarted,
		`UPDATE print_runs SET status = 'transmitting', transmission_started_at = ?
		 WHERE uid = ? AND status = 'claimed'`,
		map[string]any{"previousState": "claimed"}, ts(time.Now()), uid)
}

func (r *Repository) CancelRun(uid string) error {
	return r.transition(uid, RunCancelled, events.PrintRunCancelled,
		`UPDATE print_runs SET status = 'cancelled', retry_disposition = 'none',
		 finished_at = ? WHERE uid = ? AND status = 'queued'`,
		map[string]any{"previousState": "queued"}, ts(time.Now()), uid)
}

func (r *Repository) ResolveUncertain(uid string, resolution RunResolution) error {
	if resolution != ResolutionConfirmedPrinted && resolution != ResolutionReprintRequested {
		return fmt.Errorf("invalid uncertain resolution %q", resolution)
	}
	now := time.Now().UTC()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE print_runs SET resolution = ?, resolved_at = ?
		WHERE uid = ? AND status = 'uncertain' AND resolution IS NULL`, string(resolution), ts(now), uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStateChanged
	}
	if _, err := tx.Exec(`UPDATE jobs SET expires_at = ? WHERE uid = (SELECT job_uid FROM print_runs WHERE uid = ?)`,
		ts(now.Add(retentionWindow)), uid); err != nil {
		return err
	}
	run, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM print_runs WHERE uid = ?`, uid))
	if err != nil {
		return err
	}
	eventType := events.ManualReprintRequested
	if resolution == ResolutionConfirmedPrinted {
		eventType = events.PrintRunConfirmedPrinted
	}
	if err := r.appendRunEventTx(tx, eventType, run, map[string]any{
		"resolution": string(resolution), "physicalPrintConfirmed": resolution == ResolutionConfirmedPrinted,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if r.events != nil {
		r.events.NotifyCommit()
	}
	return nil
}

func (r *Repository) CancelTargets(jobUID string, printerIDs []string, reason string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	job, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE uid = ?`, jobUID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	changedTarget := false
	for _, printerID := range printerIDs {
		var fulfilled int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE job_uid = ? AND printer_id = ? AND
			(status = 'transmitted' OR (status = 'uncertain' AND resolution = 'confirmed_printed'))`,
			jobUID, printerID).Scan(&fulfilled); err != nil {
			return err
		}
		if fulfilled > 0 {
			return ErrTargetFulfilled
		}
		var processing int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE job_uid = ? AND printer_id = ? AND status IN ('claimed','transmitting')`, jobUID, printerID).Scan(&processing); err != nil {
			return err
		}
		if processing > 0 {
			return ErrStateChanged
		}
		res, err := tx.Exec(`UPDATE job_targets SET cancelled_at = ?, cancellation_reason = ?
			WHERE job_uid = ? AND printer_id = ? AND cancelled_at IS NULL`, ts(now), nullable(reason), jobUID, printerID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		changedTarget = true
		rows, err := tx.Query(`UPDATE print_runs SET status = 'cancelled', retry_disposition = 'none', finished_at = ?
			WHERE job_uid = ? AND printer_id = ? AND status = 'queued' RETURNING `+runCols,
			ts(now), jobUID, printerID)
		if err != nil {
			return err
		}
		var cancelled []PrintRun
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				rows.Close()
				return err
			}
			cancelled = append(cancelled, run)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE print_runs SET retry_disposition = 'suppressed'
				WHERE job_uid = ? AND printer_id = ? AND status = 'failed' AND retry_disposition = 'pending_reconnect' AND resolution IS NULL`,
			jobUID, printerID); err != nil {
			return err
		}
		for _, run := range cancelled {
			if err := r.appendRunEventTx(tx, events.PrintRunCancelled, run,
				map[string]any{"previousState": "queued", "currentState": "cancelled", "reasonProvided": reason != ""}); err != nil {
				return err
			}
		}
		if r.events != nil {
			if _, err := r.events.AppendTx(tx, events.Event{Type: events.JobTargetCancelled,
				JobUID: job.UID, JobID: job.JobID, Template: job.Template, PrinterID: printerID,
				CorrelationID: job.UID, Metadata: map[string]any{"reasonProvided": reason != ""}}); err != nil {
				return err
			}
		}
	}
	if changedTarget && r.events != nil {
		var remaining int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM job_targets WHERE job_uid = ? AND cancelled_at IS NULL`, jobUID).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			if _, err := r.events.AppendTx(tx, events.Event{Type: events.JobCancelled,
				JobUID: job.UID, JobID: job.JobID, Template: job.Template, CorrelationID: job.UID,
				Metadata: map[string]any{"reasonProvided": reason != ""}}); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE jobs SET expires_at = ? WHERE uid = ?`, ts(now.Add(retentionWindow)), jobUID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if r.events != nil {
		r.events.NotifyCommit()
	}
	return nil
}

func (r *Repository) GetDeduplication(kind, scopeUID, requestID string) (DeduplicationRecord, error) {
	var rec DeduplicationRecord
	var created string
	err := r.db.QueryRow(`SELECT request_hash, created_at FROM idempotency_records
		WHERE kind = ? AND scope_uid = ? AND request_key = ?`, kind, scopeUID, requestID).Scan(&rec.Hash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	if err != nil {
		return rec, err
	}
	rec.CreatedAt, err = parseTS("created_at", created)
	return rec, err
}

func (r *Repository) RunsForReprintRequest(jobUID, requestID string) ([]PrintRun, error) {
	return r.runsWhere(`job_uid = ? AND reprint_request_id = ? ORDER BY printer_id`, jobUID, requestID)
}

func (r *Repository) InsertManualReprints(jobUID, requestID, requestHash, reason string,
	printerIDs []string, render ReprintRenderFunc) ([]PrintRun, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	requestUID, err := newID("reprint_")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO idempotency_records
		(kind, scope_uid, request_key, request_hash, resource_uid, created_at, expires_at)
		VALUES ('manual_reprint', ?, ?, ?, ?, ?, ?)`, jobUID, requestID, requestHash,
		requestUID, ts(now), ts(now.Add(retentionWindow))); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateRequest
		}
		return nil, err
	}
	job, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE uid = ?`, jobUID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO manual_reprint_requests
		(uid, job_uid, request_id, request_hash, reason_code, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		requestUID, jobUID, requestID, requestHash, nullable(reason), ts(now)); err != nil {
		return nil, err
	}
	if r.events != nil {
		if _, err := r.events.AppendTx(tx, events.Event{Type: events.ManualReprintRequested,
			JobUID: job.UID, JobID: job.JobID, Template: job.Template, CorrelationID: requestUID,
			Metadata: map[string]any{"requestId": requestID, "printerCount": len(printerIDs)}}); err != nil {
			return nil, err
		}
	}
	ids := append([]string(nil), printerIDs...)
	sort.Strings(ids)
	created := make([]PrintRun, 0, len(ids))
	for _, printerID := range ids {
		target, err := scanTarget(tx.QueryRow(`SELECT `+targetCols+` FROM job_targets WHERE job_uid = ? AND printer_id = ?`, jobUID, printerID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE job_uid = ? AND printer_id = ? AND status IN ('queued','claimed','transmitting')`, jobUID, printerID).Scan(&active); err != nil {
			return nil, err
		}
		if active > 0 {
			return nil, ErrStateChanged
		}
		var sourceUID string
		if err := tx.QueryRow(`SELECT uid FROM print_runs WHERE job_uid = ? AND printer_id = ?
			ORDER BY run_number DESC LIMIT 1`, jobUID, printerID).Scan(&sourceUID); err != nil {
			return nil, err
		}
		var runNumber int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(run_number), 0) + 1 FROM print_runs WHERE job_uid = ? AND printer_id = ?`, jobUID, printerID).Scan(&runNumber); err != nil {
			return nil, err
		}
		hash, err := render(job, target, runNumber)
		if err != nil {
			return nil, err
		}
		uid, err := newID("run_")
		if err != nil {
			return nil, err
		}
		run := PrintRun{UID: uid, JobUID: jobUID, PrinterID: printerID, ChainUID: uid,
			RunNumber: runNumber, AttemptNumber: 1,
			Trigger: TriggerManualReprint, ContentMode: ContentReprint, ReprintOfRunUID: sourceUID,
			ReprintRequestID: requestID, ReprintReason: reason, ExpectedContentHash: hash,
			Status: RunQueued, RetryDisposition: RetryNone, CreatedAt: now}
		if err := insertRunTx(tx, run); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE job_targets SET cancelled_at = NULL, cancellation_reason = NULL WHERE job_uid = ? AND printer_id = ?`, jobUID, printerID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE print_runs SET retry_disposition = CASE
			WHEN retry_disposition = 'pending_reconnect' THEN 'suppressed' ELSE retry_disposition END,
			resolution = 'reprint_requested', resolved_at = ?
			WHERE uid = ? AND status IN ('failed','uncertain') AND resolution IS NULL`,
			ts(now), sourceUID); err != nil {
			return nil, err
		}
		if err := r.appendRunEventTx(tx, events.PrintRunQueued, run, map[string]any{
			"currentState": "queued", "manualReprintRequestId": requestID,
		}); err != nil {
			return nil, err
		}
		created = append(created, run)
	}
	if _, err := tx.Exec(`UPDATE jobs SET expires_at = ? WHERE uid = ?`, ts(now.Add(retentionWindow)), jobUID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if r.events != nil {
		r.events.NotifyCommit()
	}
	return created, nil
}

func (r *Repository) MaterializeRetries(printerID string, maxRetries int, render RetryRenderFunc) ([]PrintRun, error) {
	candidates, err := r.runsWhere(`printer_id = ? AND status = 'failed' AND retry_disposition = 'pending_reconnect' AND resolution IS NULL ORDER BY queue_sequence`, printerID)
	if err != nil {
		return nil, err
	}
	var created []PrintRun
	for _, candidate := range candidates {
		run, made, err := r.materializeRetry(candidate.UID, maxRetries, render)
		if errors.Is(err, ErrStateChanged) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return created, err
		}
		if made {
			created = append(created, run)
		}
	}
	return created, nil
}

func (r *Repository) materializeRetry(previousUID string, maxRetries int, render RetryRenderFunc) (PrintRun, bool, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return PrintRun{}, false, err
	}
	defer tx.Rollback()
	previous, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM print_runs
		WHERE uid = ? AND status = 'failed' AND retry_disposition = 'pending_reconnect' AND resolution IS NULL`, previousUID))
	if errors.Is(err, sql.ErrNoRows) {
		return PrintRun{}, false, ErrStateChanged
	}
	if err != nil {
		return PrintRun{}, false, err
	}
	depth := previous.AttemptNumber - 1
	if depth >= maxRetries {
		if _, err := tx.Exec(`UPDATE print_runs SET retry_disposition = 'exhausted' WHERE uid = ? AND retry_disposition = 'pending_reconnect'`, previousUID); err != nil {
			return PrintRun{}, false, err
		}
		previous.RetryDisposition = RetryExhausted
		if err := r.appendRunEventTx(tx, events.PrintRunRetryExhausted, previous,
			map[string]any{"retryLimit": maxRetries, "operatorAttentionRequired": true}); err != nil {
			return PrintRun{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return PrintRun{}, false, err
		}
		if r.events != nil {
			r.events.NotifyCommit()
		}
		return PrintRun{}, false, nil
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE job_uid = ? AND printer_id = ? AND status IN ('queued','claimed','transmitting')`, previous.JobUID, previous.PrinterID).Scan(&active); err != nil {
		return PrintRun{}, false, err
	}
	if active > 0 {
		return PrintRun{}, false, ErrStateChanged
	}
	job, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE uid = ?`, previous.JobUID))
	if err != nil {
		return PrintRun{}, false, err
	}
	target, err := scanTarget(tx.QueryRow(`SELECT `+targetCols+` FROM job_targets WHERE job_uid = ? AND printer_id = ?`, previous.JobUID, previous.PrinterID))
	if err != nil {
		return PrintRun{}, false, err
	}
	if target.CancelledAt != nil {
		return PrintRun{}, false, ErrStateChanged
	}
	var runNumber int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(run_number), 0) + 1 FROM print_runs WHERE job_uid = ? AND printer_id = ?`, previous.JobUID, previous.PrinterID).Scan(&runNumber); err != nil {
		return PrintRun{}, false, err
	}
	hash, err := render(job, target, previous, runNumber)
	if err != nil {
		uid, idErr := newID("run_")
		if idErr != nil {
			return PrintRun{}, false, idErr
		}
		now := time.Now().UTC()
		failed := PrintRun{UID: uid, JobUID: previous.JobUID, PrinterID: previous.PrinterID,
			ChainUID: previous.ChainUID, RunNumber: runNumber, AttemptNumber: previous.AttemptNumber + 1,
			Trigger: TriggerAutomaticRetry, ContentMode: previous.ContentMode,
			PreviousRunUID: previous.UID, Status: RunFailed, ErrorCode: "content_changed",
			ErrorMessage: err.Error(), ExpectedContentHash: previous.ExpectedContentHash,
			RetryDisposition: RetryNone, CreatedAt: now, FinishedAt: &now}
		if insertErr := insertRunTx(tx, failed); insertErr != nil {
			return PrintRun{}, false, insertErr
		}
		if _, updateErr := tx.Exec(`UPDATE print_runs SET retry_disposition = 'created' WHERE uid = ?`, previousUID); updateErr != nil {
			return PrintRun{}, false, updateErr
		}
		if err := r.appendRunEventTx(tx, events.PrintRunFailed, failed,
			map[string]any{"currentState": "failed", "failureClass": "deterministic"}); err != nil {
			return PrintRun{}, false, err
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return PrintRun{}, false, commitErr
		}
		if r.events != nil {
			r.events.NotifyCommit()
		}
		return failed, true, nil
	}
	uid, err := newID("run_")
	if err != nil {
		return PrintRun{}, false, err
	}
	now := time.Now().UTC()
	run := PrintRun{UID: uid, JobUID: previous.JobUID, PrinterID: previous.PrinterID,
		ChainUID: previous.ChainUID, RunNumber: runNumber, AttemptNumber: previous.AttemptNumber + 1,
		Trigger: TriggerAutomaticRetry, ContentMode: previous.ContentMode,
		PreviousRunUID: previous.UID, ExpectedContentHash: hash, Status: RunQueued,
		RetryDisposition: RetryNone, CreatedAt: now}
	if err := insertRunTx(tx, run); err != nil {
		return PrintRun{}, false, err
	}
	if _, err := tx.Exec(`UPDATE print_runs SET retry_disposition = 'created' WHERE uid = ? AND retry_disposition = 'pending_reconnect'`, previousUID); err != nil {
		return PrintRun{}, false, err
	}
	if err := r.appendRunEventTx(tx, events.PrintRunAutomaticRetryCreated, run,
		map[string]any{"currentState": "queued", "createdAfterReconnect": true}); err != nil {
		return PrintRun{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PrintRun{}, false, err
	}
	if r.events != nil {
		r.events.NotifyCommit()
	}
	return run, true, nil
}

func (r *Repository) ListPrinterRuns(printerID string, limit int) ([]PrintRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return r.runsWhere(`printer_id = ? ORDER BY
		CASE status WHEN 'transmitting' THEN 0 WHEN 'claimed' THEN 1 WHEN 'queued' THEN 2
		WHEN 'failed' THEN 2 WHEN 'uncertain' THEN 3 ELSE 4 END,
		CASE WHEN status IN ('transmitting','claimed','queued') THEN queue_sequence ELSE -queue_sequence END
		LIMIT ?`, printerID, limit)
}

func (r *Repository) HasActiveRuns(printerID string) (bool, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE printer_id = ? AND
		(status IN ('queued','claimed','transmitting') OR
		(status = 'failed' AND retry_disposition = 'pending_reconnect' AND resolution IS NULL))`, printerID).Scan(&n)
	return n > 0, err
}

func (r *Repository) HasProcessingRun(printerID string) (bool, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE printer_id = ? AND status IN ('claimed','transmitting')`, printerID).Scan(&n)
	return n > 0, err
}

func (r *Repository) QueueDepth() (map[string]int, error) {
	return r.countByPrinter(`status IN ('queued','claimed','transmitting') OR
		(status = 'failed' AND retry_disposition = 'pending_reconnect' AND resolution IS NULL)`)
}

func (r *Repository) AttentionCount() (map[string]int, error) {
	return r.countByPrinter(`(status = 'uncertain' AND resolution IS NULL) OR
		(status = 'failed' AND retry_disposition IN ('none','exhausted','suppressed') AND resolution IS NULL)`)
}

func (r *Repository) countByPrinter(where string) (map[string]int, error) {
	rows, err := r.db.Query(`SELECT printer_id, COUNT(*) FROM print_runs WHERE ` + where + ` GROUP BY printer_id`)
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

func (r *Repository) RecoverAbandoned() ([]PrintRun, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var out []PrintRun
	updates := []struct {
		query     string
		eventType string
		metadata  map[string]any
	}{
		{`UPDATE print_runs SET status = 'failed', retry_disposition = 'pending_reconnect',
			error_code = 'agent_restart_before_transmission', error_message = 'agent restarted before transmission began',
			finished_at = ? WHERE status = 'claimed' RETURNING ` + runCols,
			events.PrintRunFailed, map[string]any{"previousState": "claimed", "automaticRetryEligible": true}},
		{`UPDATE print_runs SET status = 'uncertain', retry_disposition = 'none',
			error_code = 'agent_restart_during_transmission', error_message = 'agent restarted after transmission began',
			finished_at = ? WHERE status = 'transmitting' RETURNING ` + runCols,
			events.PrintRunUncertain, map[string]any{"previousState": "transmitting", "operatorAttentionRequired": true}},
	}
	for _, update := range updates {
		rows, err := tx.Query(update.query, ts(now))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if err := r.appendRunEventTx(tx, update.eventType, run, update.metadata); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, run)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if r.events != nil && len(out) > 0 {
		r.events.NotifyCommit()
	}
	return out, nil
}

func (r *Repository) PurgeExpired(now time.Time) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var maxPendingAge, deadRetention, eventHighWater int64
	if err := tx.QueryRow(`SELECT max_unacknowledged_age_seconds, dead_letter_retention_seconds,
		event_disk_high_water_bytes FROM agent_settings WHERE singleton = 1`).Scan(
		&maxPendingAge, &deadRetention, &eventHighWater); err != nil {
		return err
	}
	pendingCutoff := now.Add(-time.Duration(maxPendingAge) * time.Second)
	if _, err := tx.Exec(`INSERT OR IGNORE INTO dead_letter_events
		(destination_id, event_id, event_sequence, event_type, payload_json, failed_at, reason,
		 error_code, error_message, attempt_count, expires_at)
		SELECT d.destination_id, e.event_id, e.sequence, e.event_type, e.payload_json, ?,
		'offline_duration_exhausted', 'cursor_expired', 'maximum offline buffering duration was exceeded',
		d.attempt_count, ? FROM event_deliveries d JOIN durable_events e ON e.sequence = d.event_sequence
		WHERE d.status IN ('pending','inflight') AND e.recorded_at < ?`,
		ts(now), ts(now.Add(time.Duration(deadRetention)*time.Second)), ts(pendingCutoff)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE event_deliveries SET status = 'dead_letter',
		acknowledgement_deadline = NULL, last_error_code = 'cursor_expired',
		last_error_message = 'maximum offline buffering duration was exceeded'
		WHERE status IN ('pending','inflight') AND event_sequence IN (
			SELECT sequence FROM durable_events WHERE recorded_at < ?
		)`, ts(pendingCutoff)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM jobs WHERE expires_at < ?
		AND NOT EXISTS (SELECT 1 FROM print_runs r WHERE r.job_uid = jobs.uid AND (
			r.status IN ('queued','claimed','transmitting') OR
			(r.status IN ('failed','uncertain') AND r.resolution IS NULL AND NOT EXISTS (
				SELECT 1 FROM job_targets jp WHERE jp.job_uid = r.job_uid
				AND jp.printer_id = r.printer_id AND jp.cancelled_at IS NOT NULL))))`, ts(now)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM durable_events WHERE expires_at < ? AND NOT EXISTS (
		SELECT 1 FROM event_deliveries d WHERE d.event_sequence = durable_events.sequence
		AND d.status IN ('pending','inflight'))`, ts(now)); err != nil {
		return err
	}
	var pageCount, pageSize int64
	if err := tx.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return err
	}
	if err := tx.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return err
	}
	if pageCount*pageSize >= eventHighWater {
		if _, err := tx.Exec(`DELETE FROM durable_events WHERE sequence IN (
			SELECT sequence FROM durable_events e WHERE NOT EXISTS (
				SELECT 1 FROM event_deliveries d WHERE d.event_sequence=e.sequence
				AND d.status IN ('pending','inflight'))
			ORDER BY sequence LIMIT MAX(1, (SELECT COUNT(*) FROM durable_events) / 4)
		)`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM idempotency_records WHERE expires_at < ?`, ts(now)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM dead_letter_events WHERE expires_at < ?`, ts(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func runEvent(eventType string, job Job, run PrintRun, metadata map[string]any) events.Event {
	return events.Event{
		Type: eventType, PrinterID: run.PrinterID, JobUID: job.UID, JobID: job.JobID,
		Template: job.Template, RunUID: run.UID, RunNumber: run.RunNumber,
		AttemptNumber: run.AttemptNumber, Trigger: string(run.Trigger),
		PreviousRunUID: run.PreviousRunUID, ReprintOfRunUID: run.ReprintOfRunUID,
		CorrelationID: job.UID, Metadata: metadata,
	}
}

func (r *Repository) appendRunEventTx(tx *sql.Tx, eventType string, run PrintRun, metadata map[string]any) error {
	if r.events == nil {
		return nil
	}
	job, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE uid = ?`, run.JobUID))
	if err != nil {
		return err
	}
	event := runEvent(eventType, job, run, metadata)
	if run.ErrorCode != "" {
		event.Error = &events.Error{Code: run.ErrorCode, Message: run.ErrorMessage,
			Retryable: run.RetryDisposition == RetryPendingReconnect}
	}
	_, err = r.events.AppendTx(tx, event)
	return err
}

func (r *Repository) Ping() error {
	var one int
	return r.db.QueryRow(`SELECT 1`).Scan(&one)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
