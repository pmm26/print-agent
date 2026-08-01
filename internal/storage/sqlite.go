// Package storage owns the SQLite database: opening, pragmas, and embedded
// schema migrations. SQLite is the authoritative print queue — a job exists
// once its transaction commits, and only then.
package storage

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=auto_vacuum(INCREMENTAL)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection sidesteps SQLITE_BUSY between the API and the
	// printer workers; throughput needs here are trivial (a few writes/sec).
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		db.Close()
		if err != nil {
			return nil, fmt.Errorf("database integrity check: %w", err)
		}
		return nil, fmt.Errorf("database integrity check failed: %s", integrity)
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(name, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("secure database file %s: %w", name, err)
		}
	}
	return db, nil
}

var ErrIncompatibleSchema = errors.New("pre-release database schema is incompatible; stop the agent and delete its data directory before restarting")

func migrate(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		rows, err := db.Query(`PRAGMA table_info(schema_migrations)`)
		if err != nil {
			return err
		}
		hasChecksum := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
				rows.Close()
				return err
			}
			if name == "checksum" {
				hasChecksum = true
			}
		}
		rows.Close()
		if !hasChecksum {
			return ErrIncompatibleSchema
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(sqlBytes)
		checksum := hex.EncodeToString(sum[:])
		var stored string
		err = db.QueryRow(`SELECT checksum FROM schema_migrations WHERE version = ?`, name).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if stored != checksum {
				return fmt.Errorf("migration %s checksum changed", name)
			}
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, ?)`,
			name, checksum, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// IncrementalVacuum returns free pages to the filesystem after retention.
func IncrementalVacuum(db *sql.DB) error {
	_, err := db.Exec(`PRAGMA incremental_vacuum`)
	return err
}
