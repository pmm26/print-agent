// Package diagnostics assembles the health report, recent event log, and
// the sanitized export bundle for support.
package diagnostics

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"time"

	"print-agent/internal/events"
)

const EventRetention = 7 * 24 * time.Hour

// Version is stamped at build time via -ldflags "-X ...".
var Version = "dev"

type EventRow struct {
	ID        int64     `json:"id"`
	RunUID    string    `json:"runUid,omitempty"`
	PrinterID string    `json:"printerId,omitempty"`
	Type      string    `json:"type"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type EventFilter struct {
	PrinterID string
	EventType string
	RunUID    string
	Query     string
	From      *time.Time
	To        *time.Time
	Before    *time.Time
	BeforeID  int64
	Limit     int
	Now       time.Time
}

type Service struct {
	db        *sql.DB
	events    *events.Store
	dbPath    string
	startedAt time.Time
}

func NewService(db *sql.DB, dbPath string) *Service {
	store, _ := events.NewStore(db)
	return &Service{db: db, events: store, dbPath: dbPath, startedAt: time.Now()}
}

// PersistEvent records a sanitized operational event. Domain transitions use
// events.Store.AppendTx directly so their event and state change are atomic.
func (s *Service) PersistEvent(eventType, printerID, runUID, message string, at time.Time) error {
	_, err := s.events.Append(context.Background(), events.Event{Type: eventType, PrinterID: printerID,
		RunUID: runUID, Message: message, OccurredAt: at, Durability: events.DurabilityOperational})
	return err
}

// RecentEvents returns the newest events, newest first.
func (s *Service) RecentEvents(limit int) ([]EventRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT sequence, COALESCE(run_uid, ''), COALESCE(printer_id, ''),
		event_type, COALESCE(safe_message, ''), occurred_at
		FROM durable_events WHERE occurred_at >= ? ORDER BY sequence DESC LIMIT ?`,
		time.Now().UTC().Add(-EventRetention).Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	return readEventRows(rows)
}

func (s *Service) PrinterEvents(filter EventFilter) ([]EventRow, error) {
	if filter.Limit <= 0 || filter.Limit > 1001 {
		filter.Limit = 100
	}
	if filter.Now.IsZero() {
		filter.Now = time.Now().UTC()
	}
	from := filter.Now.Add(-EventRetention)
	if filter.From != nil && filter.From.After(from) {
		from = filter.From.UTC()
	}
	where := []string{`occurred_at >= ?`, `category IN ('printer','print_run','config')`}
	args := []any{from.Format(time.RFC3339Nano)}
	if filter.To != nil {
		where = append(where, "occurred_at <= ?")
		args = append(args, filter.To.UTC().Format(time.RFC3339Nano))
	}
	if filter.PrinterID != "" {
		where = append(where, "printer_id = ?")
		args = append(args, filter.PrinterID)
	}
	if filter.EventType != "" {
		where = append(where, "event_type = ?")
		args = append(args, filter.EventType)
	}
	if filter.RunUID != "" {
		where = append(where, "run_uid = ?")
		args = append(args, filter.RunUID)
	}
	if filter.Query != "" {
		where = append(where, `(LOWER(event_type) LIKE ? ESCAPE '\' OR LOWER(COALESCE(safe_message, '')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(printer_id, '')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(run_uid, '')) LIKE ? ESCAPE '\')`)
		term := "%" + escapeLike(strings.ToLower(filter.Query)) + "%"
		args = append(args, term, term, term, term)
	}
	if filter.Before != nil {
		where = append(where, "(occurred_at < ? OR (occurred_at = ? AND sequence < ?))")
		stamp := filter.Before.UTC().Format(time.RFC3339Nano)
		args = append(args, stamp, stamp, filter.BeforeID)
	}
	args = append(args, filter.Limit)
	rows, err := s.db.Query(`SELECT sequence, COALESCE(run_uid, ''), COALESCE(printer_id, ''),
		event_type, COALESCE(safe_message, ''), occurred_at
		FROM durable_events WHERE `+strings.Join(where, " AND ")+`
		ORDER BY occurred_at DESC, sequence DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return readEventRows(rows)
}

func readEventRows(rows *sql.Rows) ([]EventRow, error) {
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var created string
		if err := rows.Scan(&e.ID, &e.RunUID, &e.PrinterID, &e.Type, &e.Message, &created); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		e.CreatedAt = parsed
		out = append(out, e)
	}
	return out, rows.Err()
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

// Report is the diagnostics page payload. PrinterStatuses and settings are
// added by the API layer, which has access to the manager.
type Report struct {
	Version       string    `json:"version"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	GoVersion     string    `json:"goVersion"`
	StartedAt     time.Time `json:"startedAt"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
	DatabasePath  string    `json:"databasePath"`
	DatabaseOK    bool      `json:"databaseOk"`
}

func (s *Service) Report() Report {
	return Report{
		Version:       Version,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		GoVersion:     runtime.Version(),
		StartedAt:     s.startedAt,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		DatabasePath:  s.dbPath,
		DatabaseOK:    s.db.Ping() == nil,
	}
}

// WriteExportZip writes a support bundle: the report, recent events, and
// sanitized extras supplied by the caller (printer statuses, settings —
// never authentication secrets).
func (s *Service) WriteExportZip(w io.Writer, extras map[string]any) error {
	zw := zip.NewWriter(w)
	writeJSON := func(name string, v any) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if err := writeJSON("report.json", s.Report()); err != nil {
		return err
	}
	events, err := s.RecentEvents(1000)
	if err != nil {
		return err
	}
	if err := writeJSON("events.json", events); err != nil {
		return err
	}
	for name, v := range extras {
		if err := writeJSON(name, v); err != nil {
			return err
		}
	}
	return zw.Close()
}
