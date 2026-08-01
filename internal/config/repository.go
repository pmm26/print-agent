package config

import (
	"database/sql"
	"errors"
	"time"
)

// Repository persists printer configurations and agent settings.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

var ErrNotFound = errors.New("not found")

const printerCols = `id, display_name, enabled, transport, COALESCE(device_address, ''),
	COALESCE(endpoint, ''), baud_rate, data_bits, stop_bits, parity, paper_width_mm,
	characters_per_line, encoding, auto_reconnect, status_probe_enabled, created_at, updated_at`

func scanPrinter(row interface{ Scan(...any) error }) (PrinterConfig, error) {
	var p PrinterConfig
	var created, updated, transport string
	err := row.Scan(&p.ID, &p.DisplayName, &p.Enabled, &transport, &p.DeviceAddress,
		&p.Endpoint, &p.BaudRate, &p.DataBits, &p.StopBits, &p.Parity, &p.PaperWidthMm,
		&p.CharactersPerLine, &p.Encoding, &p.AutoReconnect, &p.StatusProbeEnabled, &created, &updated)
	if err != nil {
		return p, err
	}
	p.Transport = TransportKind(transport)
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

// ListPrinters returns all configured printers ordered by ID.
func (r *Repository) ListPrinters() ([]PrinterConfig, error) {
	rows, err := r.db.Query(`SELECT ` + printerCols + ` FROM printers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrinterConfig
	for rows.Next() {
		p, err := scanPrinter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPrinter returns one printer or ErrNotFound.
func (r *Repository) GetPrinter(id string) (PrinterConfig, error) {
	row := r.db.QueryRow(`SELECT `+printerCols+` FROM printers WHERE id = ?`, id)
	p, err := scanPrinter(row)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// SavePrinter inserts or updates a printer configuration.
func (r *Repository) SavePrinter(p PrinterConfig) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := r.db.Exec(`INSERT INTO printers
		(id, display_name, enabled, transport, device_address, endpoint, baud_rate, data_bits,
		 stop_bits, parity, paper_width_mm, characters_per_line, encoding, auto_reconnect,
		 status_probe_enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		 display_name = excluded.display_name,
		 enabled = excluded.enabled,
		 transport = excluded.transport,
		 device_address = excluded.device_address,
		 endpoint = excluded.endpoint,
		 baud_rate = excluded.baud_rate,
		 data_bits = excluded.data_bits,
		 stop_bits = excluded.stop_bits,
		 parity = excluded.parity,
		 paper_width_mm = excluded.paper_width_mm,
		 characters_per_line = excluded.characters_per_line,
		 encoding = excluded.encoding,
		 auto_reconnect = excluded.auto_reconnect,
		 status_probe_enabled = excluded.status_probe_enabled,
		 updated_at = excluded.updated_at`,
		p.ID, p.DisplayName, p.Enabled, string(p.Transport), p.DeviceAddress, p.Endpoint,
		p.BaudRate, p.DataBits, p.StopBits, p.Parity, p.PaperWidthMm, p.CharactersPerLine,
		p.Encoding, p.AutoReconnect, p.StatusProbeEnabled, now, now)
	return err
}

// DeletePrinter removes a printer configuration. Historical deliveries and
// events keep their printer_id strings.
func (r *Repository) DeletePrinter(id string) error {
	res, err := r.db.Exec(`DELETE FROM printers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled flips a printer's enabled flag.
func (r *Repository) SetEnabled(id string, enabled bool) error {
	res, err := r.db.Exec(`UPDATE printers SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetSetting returns a settings value, or "" when unset.
func (r *Repository) GetSetting(key string) (string, error) {
	var v string
	err := r.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a settings value.
func (r *Repository) SetSetting(key, value string) error {
	_, err := r.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
