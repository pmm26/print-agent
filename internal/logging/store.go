package logging

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const Retention = 2 * time.Hour

// SQLite compares these timestamps as text, so every persisted and bound
// value has the same width. The row ID orders records within a millisecond.
const sqliteTimeLayout = "2006-01-02T15:04:05.000Z"

type Record struct {
	ID         int64          `json:"id"`
	CreatedAt  time.Time      `json:"createdAt"`
	Level      string         `json:"level"`
	Message    string         `json:"message"`
	PrinterID  string         `json:"printerId,omitempty"`
	RunUID     string         `json:"runUid,omitempty"`
	Attributes map[string]any `json:"attributes"`
}

type Filter struct {
	Levels    []string
	Query     string
	PrinterID string
	RunUID    string
	From      *time.Time
	To        *time.Time
	Before    *time.Time
	BeforeID  int64
	Limit     int
	Now       time.Time
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Insert(record Record) error {
	attrs, err := json.Marshal(record.Attributes)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO system_logs
		(created_at, level, message, printer_id, run_uid, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?)`, record.CreatedAt.UTC().Format(sqliteTimeLayout),
		record.Level, record.Message, nullable(record.PrinterID), nullable(record.RunUID), string(attrs))
	return err
}

func (s *Store) Query(filter Filter) ([]Record, error) {
	if filter.Limit <= 0 || filter.Limit > 201 {
		filter.Limit = 100
	}
	if filter.Now.IsZero() {
		filter.Now = time.Now().UTC()
	}
	from := filter.Now.Add(-Retention)
	if filter.From != nil && filter.From.After(from) {
		from = filter.From.UTC()
	}

	var where []string
	args := []any{from.Format(sqliteTimeLayout)}
	where = append(where, "created_at >= ?")
	if filter.To != nil {
		where = append(where, "created_at <= ?")
		args = append(args, filter.To.UTC().Format(sqliteTimeLayout))
	}
	if len(filter.Levels) > 0 {
		placeholders := make([]string, len(filter.Levels))
		for i, level := range filter.Levels {
			placeholders[i] = "?"
			args = append(args, level)
		}
		where = append(where, "level IN ("+strings.Join(placeholders, ",")+")")
	}
	if filter.PrinterID != "" {
		where = append(where, "printer_id = ?")
		args = append(args, filter.PrinterID)
	}
	if filter.RunUID != "" {
		where = append(where, "run_uid = ?")
		args = append(args, filter.RunUID)
	}
	if filter.Query != "" {
		where = append(where, `(LOWER(message) LIKE ? ESCAPE '\' OR LOWER(attrs_json) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(printer_id, '')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(run_uid, '')) LIKE ? ESCAPE '\')`)
		term := "%" + escapeLike(strings.ToLower(filter.Query)) + "%"
		args = append(args, term, term, term, term)
	}
	if filter.Before != nil {
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		stamp := filter.Before.UTC().Format(sqliteTimeLayout)
		args = append(args, stamp, stamp, filter.BeforeID)
	}
	args = append(args, filter.Limit)

	rows, err := s.db.Query(`SELECT id, created_at, level, message,
		COALESCE(printer_id, ''), COALESCE(run_uid, ''), attrs_json
		FROM system_logs WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		var record Record
		var created, attrs string
		if err := rows.Scan(&record.ID, &created, &record.Level, &record.Message,
			&record.PrinterID, &record.RunUID, &attrs); err != nil {
			return nil, err
		}
		if record.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("parse system log time: %w", err)
		}
		if err := json.Unmarshal([]byte(attrs), &record.Attributes); err != nil {
			return nil, fmt.Errorf("parse system log attributes: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) Purge(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM system_logs WHERE created_at < ?`,
		now.UTC().Add(-Retention).Format(sqliteTimeLayout))
	return err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}
