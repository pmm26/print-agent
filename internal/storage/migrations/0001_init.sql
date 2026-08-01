CREATE TABLE printers (
    id                   TEXT PRIMARY KEY,
    display_name         TEXT NOT NULL,
    enabled              INTEGER NOT NULL DEFAULT 1,
    transport            TEXT NOT NULL DEFAULT 'bluetooth-serial',
    device_address       TEXT,
    endpoint             TEXT,
    baud_rate            INTEGER NOT NULL DEFAULT 9600,
    data_bits            INTEGER NOT NULL DEFAULT 8,
    stop_bits            INTEGER NOT NULL DEFAULT 1,
    parity               TEXT NOT NULL DEFAULT 'none',
    paper_width_mm       INTEGER NOT NULL DEFAULT 58,
    characters_per_line  INTEGER NOT NULL DEFAULT 32,
    encoding             TEXT NOT NULL DEFAULT 'CP858',
    auto_reconnect       INTEGER NOT NULL DEFAULT 1,
    status_probe_enabled INTEGER NOT NULL DEFAULT 0,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);

CREATE TABLE print_jobs (
    id              TEXT PRIMARY KEY,
    external_job_id TEXT NOT NULL UNIQUE,
    source          TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    accepted_at     TEXT NOT NULL
);

CREATE TABLE print_deliveries (
    id                   TEXT PRIMARY KEY,
    external_delivery_id TEXT NOT NULL UNIQUE,
    job_id               TEXT NOT NULL REFERENCES print_jobs(id),
    printer_id           TEXT NOT NULL,
    template             TEXT NOT NULL,
    payload_json         TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'queued',
    attempt_count        INTEGER NOT NULL DEFAULT 0,
    bytes_written        INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT,
    reprint_of           TEXT,
    created_at           TEXT NOT NULL,
    started_at           TEXT,
    transmitted_at       TEXT,
    resolved_at          TEXT
);
CREATE INDEX idx_deliveries_printer_status ON print_deliveries(printer_id, status);
CREATE INDEX idx_deliveries_job ON print_deliveries(job_id);

CREATE TABLE print_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id  TEXT,
    printer_id   TEXT,
    event_type   TEXT NOT NULL,
    message      TEXT,
    details_json TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX idx_events_created ON print_events(created_at);

CREATE TABLE client_tokens (
    id             TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL,
    label          TEXT,
    allowed_origin TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    last_used_at   TEXT,
    revoked_at     TEXT
);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
