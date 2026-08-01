package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const retentionWindow = 48 * time.Hour

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

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

const runCols = `queue_sequence, uid, job_uid, printer_id, run_number, trigger, content_mode,
	COALESCE(previous_run_uid, ''), COALESCE(reprint_of_run_uid, ''),
	COALESCE(reprint_request_id, ''), COALESCE(reprint_reason, ''), expected_content_hash,
	status, retryable, COALESCE(resolution, ''), COALESCE(error_code, ''),
	COALESCE(error_message, ''), bytes_accepted, created_at, started_at, finished_at,
	transmitted_at, resolved_at`

func scanRun(row interface{ Scan(...any) error }) (PrintRun, error) {
	var run PrintRun
	var trigger, mode, status, resolution, created string
	var started, finished, transmitted, resolved sql.NullString
	if err := row.Scan(&run.QueueSequence, &run.UID, &run.JobUID, &run.PrinterID,
		&run.RunNumber, &trigger, &mode, &run.PreviousRunUID, &run.ReprintOfRunUID,
		&run.ReprintRequestID, &run.ReprintReason, &run.ExpectedContentHash, &status,
		&run.Retryable, &resolution, &run.ErrorCode, &run.ErrorMessage, &run.BytesAccepted,
		&created, &started, &finished, &transmitted, &resolved); err != nil {
		return run, err
	}
	run.Trigger = PrintRunTrigger(trigger)
	run.ContentMode = ContentMode(mode)
	run.Status = PrintRunStatus(status)
	run.Resolution = RunResolution(resolution)
	var err error
	if run.CreatedAt, err = parseTS("created_at", created); err != nil {
		return run, err
	}
	if run.StartedAt, err = parseTSPtr("started_at", started); err != nil {
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
		if _, err := tx.Exec(`INSERT INTO job_printers
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
	return tx.Commit()
}

func insertRunTx(tx *sql.Tx, run PrintRun) error {
	_, err := tx.Exec(`INSERT INTO print_runs
		(uid, job_uid, printer_id, run_number, trigger, content_mode, previous_run_uid,
		 reprint_of_run_uid, reprint_request_id, reprint_reason, expected_content_hash,
		 status, retryable, resolution, error_code, error_message, bytes_accepted,
		 created_at, started_at, finished_at, transmitted_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.UID, run.JobUID, run.PrinterID, run.RunNumber, string(run.Trigger), string(run.ContentMode),
		nullable(run.PreviousRunUID), nullable(run.ReprintOfRunUID), nullable(run.ReprintRequestID),
		nullable(run.ReprintReason), run.ExpectedContentHash, string(run.Status), run.Retryable,
		nullable(string(run.Resolution)), nullable(run.ErrorCode), nullable(run.ErrorMessage),
		run.BytesAccepted, ts(run.CreatedAt), timeOrNil(run.StartedAt), timeOrNil(run.FinishedAt),
		timeOrNil(run.TransmittedAt), timeOrNil(run.ResolvedAt))
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
	rows, err := r.db.Query(`SELECT `+targetCols+` FROM job_printers WHERE job_uid = ? ORDER BY target_order`, jobUID)
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
	target, err := scanTarget(r.db.QueryRow(`SELECT `+targetCols+` FROM job_printers
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
			AND (ar.status = 'uncertain' OR (ar.status = 'failed' AND ar.retryable = 0))
			AND (ar.content_mode = 'reprint' OR (
				NOT EXISTS (SELECT 1 FROM print_runs ok WHERE ok.job_uid = ar.job_uid
					AND ok.printer_id = ar.printer_id AND
					(ok.status = 'transmitted' OR (ok.status = 'uncertain' AND ok.resolution = 'confirmed_printed')))
				AND NOT EXISTS (SELECT 1 FROM job_printers cp WHERE cp.job_uid = ar.job_uid
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
	run, err := scanRun(r.db.QueryRow(`UPDATE print_runs
		SET status = 'processing', started_at = ?
		WHERE queue_sequence = (
			SELECT queue_sequence FROM print_runs
			WHERE printer_id = ? AND status = 'queued'
			ORDER BY queue_sequence LIMIT 1
		)
		RETURNING `+runCols, ts(time.Now()), printerID))
	if errors.Is(err, sql.ErrNoRows) {
		return run, ErrNotFound
	}
	return run, err
}

func (r *Repository) transition(uid string, target PrintRunStatus, query string, args ...any) error {
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
	if n != 1 {
		var current string
		if err := tx.QueryRow(`SELECT status FROM print_runs WHERE uid = ?`, uid).Scan(&current); err != nil || PrintRunStatus(current) != target {
			return ErrStateChanged
		}
	}
	if _, err := tx.Exec(`UPDATE jobs SET expires_at = ? WHERE uid = (SELECT job_uid FROM print_runs WHERE uid = ?)`,
		ts(time.Now().UTC().Add(retentionWindow)), uid); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) MarkTransmitted(uid string, bytesAccepted int) error {
	now := time.Now().UTC()
	return r.transition(uid, RunTransmitted, `UPDATE print_runs SET status = 'transmitted', retryable = 0,
		bytes_accepted = ?, transmitted_at = ?, finished_at = ?, error_code = NULL, error_message = NULL
		WHERE uid = ? AND status = 'processing'`, bytesAccepted, ts(now), ts(now), uid)
}

func (r *Repository) MarkFailed(uid string, bytesAccepted int, code, message string, retryable bool) error {
	return r.transition(uid, RunFailed, `UPDATE print_runs SET status = 'failed', retryable = ?,
		bytes_accepted = ?, error_code = ?, error_message = ?, finished_at = ?
		WHERE uid = ? AND status = 'processing'`, retryable, bytesAccepted, code, message, ts(time.Now()), uid)
}

func (r *Repository) MarkUncertain(uid string, bytesAccepted int, code, message string) error {
	return r.transition(uid, RunUncertain, `UPDATE print_runs SET status = 'uncertain', retryable = 0,
		bytes_accepted = ?, error_code = ?, error_message = ?, finished_at = ?
		WHERE uid = ? AND status = 'processing'`, bytesAccepted, code, message, ts(time.Now()), uid)
}

func (r *Repository) CancelRun(uid string) error {
	return r.transition(uid, RunCancelled, `UPDATE print_runs SET status = 'cancelled', retryable = 0,
		finished_at = ? WHERE uid = ? AND status = 'queued'`, ts(time.Now()), uid)
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
	return tx.Commit()
}

func (r *Repository) CancelTargets(jobUID string, printerIDs []string, reason string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
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
		if err := tx.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE job_uid = ? AND printer_id = ? AND status = 'processing'`, jobUID, printerID).Scan(&processing); err != nil {
			return err
		}
		if processing > 0 {
			return ErrStateChanged
		}
		res, err := tx.Exec(`UPDATE job_printers SET cancelled_at = ?, cancellation_reason = ?
			WHERE job_uid = ? AND printer_id = ?`, ts(now), nullable(reason), jobUID, printerID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		if _, err := tx.Exec(`UPDATE print_runs SET status = 'cancelled', retryable = 0, finished_at = ?
			WHERE job_uid = ? AND printer_id = ? AND status = 'queued'`, ts(now), jobUID, printerID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE print_runs SET retryable = 0
			WHERE job_uid = ? AND printer_id = ? AND status = 'failed' AND retryable = 1 AND resolution IS NULL`,
			jobUID, printerID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE jobs SET expires_at = ? WHERE uid = ?`, ts(now.Add(retentionWindow)), jobUID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) GetDeduplication(kind, scopeUID, requestID string) (DeduplicationRecord, error) {
	var rec DeduplicationRecord
	var created string
	err := r.db.QueryRow(`SELECT request_hash, created_at FROM request_deduplication
		WHERE kind = ? AND scope_uid = ? AND request_id = ?`, kind, scopeUID, requestID).Scan(&rec.Hash, &created)
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
	if _, err := tx.Exec(`INSERT INTO request_deduplication
		(kind, scope_uid, request_id, request_hash, created_at) VALUES ('manual_reprint', ?, ?, ?, ?)`,
		jobUID, requestID, requestHash, ts(now)); err != nil {
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
	ids := append([]string(nil), printerIDs...)
	sort.Strings(ids)
	created := make([]PrintRun, 0, len(ids))
	for _, printerID := range ids {
		target, err := scanTarget(tx.QueryRow(`SELECT `+targetCols+` FROM job_printers WHERE job_uid = ? AND printer_id = ?`, jobUID, printerID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE job_uid = ? AND printer_id = ? AND status IN ('queued','processing')`, jobUID, printerID).Scan(&active); err != nil {
			return nil, err
		}
		if active > 0 {
			return nil, ErrStateChanged
		}
		var initialUID string
		if err := tx.QueryRow(`SELECT uid FROM print_runs WHERE job_uid = ? AND printer_id = ? AND trigger = 'initial'`, jobUID, printerID).Scan(&initialUID); err != nil {
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
		run := PrintRun{UID: uid, JobUID: jobUID, PrinterID: printerID, RunNumber: runNumber,
			Trigger: TriggerManualReprint, ContentMode: ContentReprint, ReprintOfRunUID: initialUID,
			ReprintRequestID: requestID, ReprintReason: reason, ExpectedContentHash: hash,
			Status: RunQueued, CreatedAt: now}
		if err := insertRunTx(tx, run); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE job_printers SET cancelled_at = NULL, cancellation_reason = NULL WHERE job_uid = ? AND printer_id = ?`, jobUID, printerID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE print_runs SET retryable = 0, resolution = 'reprint_requested', resolved_at = ?
			WHERE job_uid = ? AND printer_id = ? AND status IN ('failed','uncertain') AND resolution IS NULL`,
			ts(now), jobUID, printerID); err != nil {
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
	return created, nil
}

func (r *Repository) MaterializeRetries(printerID string, maxRetries int, render RetryRenderFunc) ([]PrintRun, error) {
	candidates, err := r.runsWhere(`printer_id = ? AND status = 'failed' AND retryable = 1 AND resolution IS NULL ORDER BY queue_sequence`, printerID)
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
		WHERE uid = ? AND status = 'failed' AND retryable = 1 AND resolution IS NULL`, previousUID))
	if errors.Is(err, sql.ErrNoRows) {
		return PrintRun{}, false, ErrStateChanged
	}
	if err != nil {
		return PrintRun{}, false, err
	}
	depth := 0
	cursor := previous
	for cursor.Trigger == TriggerAutomaticRetry {
		depth++
		if cursor.PreviousRunUID == "" {
			break
		}
		cursor, err = scanRun(tx.QueryRow(`SELECT `+runCols+` FROM print_runs WHERE uid = ?`, cursor.PreviousRunUID))
		if err != nil {
			return PrintRun{}, false, err
		}
	}
	if depth >= maxRetries {
		if _, err := tx.Exec(`UPDATE print_runs SET retryable = 0 WHERE uid = ? AND retryable = 1`, previousUID); err != nil {
			return PrintRun{}, false, err
		}
		return PrintRun{}, false, tx.Commit()
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE job_uid = ? AND printer_id = ? AND status IN ('queued','processing')`, previous.JobUID, previous.PrinterID).Scan(&active); err != nil {
		return PrintRun{}, false, err
	}
	if active > 0 {
		return PrintRun{}, false, ErrStateChanged
	}
	job, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE uid = ?`, previous.JobUID))
	if err != nil {
		return PrintRun{}, false, err
	}
	target, err := scanTarget(tx.QueryRow(`SELECT `+targetCols+` FROM job_printers WHERE job_uid = ? AND printer_id = ?`, previous.JobUID, previous.PrinterID))
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
			RunNumber: runNumber, Trigger: TriggerAutomaticRetry, ContentMode: previous.ContentMode,
			PreviousRunUID: previous.UID, Status: RunFailed, ErrorCode: "content_changed",
			ErrorMessage: err.Error(), CreatedAt: now, FinishedAt: &now}
		if insertErr := insertRunTx(tx, failed); insertErr != nil {
			return PrintRun{}, false, insertErr
		}
		if _, updateErr := tx.Exec(`UPDATE print_runs SET retryable = 0 WHERE uid = ?`, previousUID); updateErr != nil {
			return PrintRun{}, false, updateErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return PrintRun{}, false, commitErr
		}
		return failed, true, nil
	}
	uid, err := newID("run_")
	if err != nil {
		return PrintRun{}, false, err
	}
	now := time.Now().UTC()
	run := PrintRun{UID: uid, JobUID: previous.JobUID, PrinterID: previous.PrinterID,
		RunNumber: runNumber, Trigger: TriggerAutomaticRetry, ContentMode: previous.ContentMode,
		PreviousRunUID: previous.UID, ExpectedContentHash: hash, Status: RunQueued, CreatedAt: now}
	if err := insertRunTx(tx, run); err != nil {
		return PrintRun{}, false, err
	}
	if _, err := tx.Exec(`UPDATE print_runs SET retryable = 0 WHERE uid = ? AND retryable = 1`, previousUID); err != nil {
		return PrintRun{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PrintRun{}, false, err
	}
	return run, true, nil
}

func (r *Repository) ListPrinterRuns(printerID string, limit int) ([]PrintRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return r.runsWhere(`printer_id = ? ORDER BY
		CASE status WHEN 'processing' THEN 0 WHEN 'queued' THEN 1
		WHEN 'failed' THEN 2 WHEN 'uncertain' THEN 3 ELSE 4 END,
		CASE WHEN status IN ('processing','queued') THEN queue_sequence ELSE -queue_sequence END
		LIMIT ?`, printerID, limit)
}

func (r *Repository) HasActiveRuns(printerID string) (bool, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE printer_id = ? AND
		(status IN ('queued','processing') OR (status = 'failed' AND retryable = 1 AND resolution IS NULL))`, printerID).Scan(&n)
	return n > 0, err
}

func (r *Repository) HasProcessingRun(printerID string) (bool, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM print_runs WHERE printer_id = ? AND status = 'processing'`, printerID).Scan(&n)
	return n > 0, err
}

func (r *Repository) QueueDepth() (map[string]int, error) {
	return r.countByPrinter(`status IN ('queued','processing') OR (status = 'failed' AND retryable = 1 AND resolution IS NULL)`)
}

func (r *Repository) AttentionCount() (map[string]int, error) {
	return r.countByPrinter(`(status = 'uncertain' AND resolution IS NULL) OR
		(status = 'failed' AND retryable = 0 AND resolution IS NULL)`)
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
	rows, err := r.db.Query(`UPDATE print_runs SET status = 'uncertain', retryable = 0,
		error_code = 'agent_restart_during_processing', error_message = 'agent restarted while processing',
		finished_at = ? WHERE status = 'processing' RETURNING `+runCols, ts(time.Now()))
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

func (r *Repository) PurgeExpired(now time.Time) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM jobs WHERE expires_at < ?
		AND NOT EXISTS (SELECT 1 FROM print_runs r WHERE r.job_uid = jobs.uid AND (
			r.status IN ('queued','processing') OR
			(r.status IN ('failed','uncertain') AND r.resolution IS NULL AND NOT EXISTS (
				SELECT 1 FROM job_printers jp WHERE jp.job_uid = r.job_uid
				AND jp.printer_id = r.printer_id AND jp.cancelled_at IS NOT NULL))))`, ts(now)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM print_events WHERE created_at < ?`, ts(now.Add(-retentionWindow))); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) Ping() error {
	var one int
	return r.db.QueryRow(`SELECT 1`).Scan(&one)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
