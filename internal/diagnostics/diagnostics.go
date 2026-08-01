// Package diagnostics assembles the health report, recent event log, and
// the sanitized export bundle for support.
package diagnostics

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"io"
	"runtime"
	"time"
)

// Version is stamped at build time via -ldflags "-X ...".
var Version = "dev"

type EventRow struct {
	ID         int64  `json:"id"`
	DeliveryID string `json:"deliveryId,omitempty"`
	PrinterID  string `json:"printerId,omitempty"`
	Type       string `json:"type"`
	Message    string `json:"message,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

type Service struct {
	db        *sql.DB
	dbPath    string
	startedAt time.Time
}

func NewService(db *sql.DB, dbPath string) *Service {
	return &Service{db: db, dbPath: dbPath, startedAt: time.Now()}
}

// PersistEvent stores one bus event; wired as an event-bus subscriber.
func (s *Service) PersistEvent(eventType, printerID, deliveryID, message string, at time.Time) {
	s.db.Exec(`INSERT INTO print_events (delivery_id, printer_id, event_type, message, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		nullable(deliveryID), nullable(printerID), eventType, nullable(message),
		at.UTC().Format(time.RFC3339Nano))
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RecentEvents returns the newest events, newest first.
func (s *Service) RecentEvents(limit int) ([]EventRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, COALESCE(delivery_id, ''), COALESCE(printer_id, ''),
		event_type, COALESCE(message, ''), created_at
		FROM print_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.ID, &e.DeliveryID, &e.PrinterID, &e.Type, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
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
	if err == nil {
		if err := writeJSON("events.json", events); err != nil {
			return err
		}
	}
	for name, v := range extras {
		if err := writeJSON(name, v); err != nil {
			return err
		}
	}
	return zw.Close()
}
