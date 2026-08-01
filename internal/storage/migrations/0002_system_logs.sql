CREATE TABLE system_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at  TEXT NOT NULL,
    level       TEXT NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    message     TEXT NOT NULL,
    printer_id  TEXT,
    run_uid     TEXT,
    attrs_json  TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_system_logs_created ON system_logs(created_at DESC, id DESC);
CREATE INDEX idx_system_logs_level_created ON system_logs(level, created_at DESC, id DESC);
CREATE INDEX idx_system_logs_printer_created ON system_logs(printer_id, created_at DESC, id DESC)
    WHERE printer_id IS NOT NULL;
CREATE INDEX idx_system_logs_run_created ON system_logs(run_uid, created_at DESC, id DESC)
    WHERE run_uid IS NOT NULL;

-- Older builds used variable-width RFC3339 fractions. Normalize retained
-- printer events so text ordering and cursor comparisons remain exact.
UPDATE print_events
SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', created_at);

CREATE INDEX idx_print_events_printer_created ON print_events(printer_id, created_at DESC, id DESC);
CREATE INDEX idx_print_events_type_created ON print_events(event_type, created_at DESC, id DESC);
CREATE INDEX idx_print_events_run_created ON print_events(run_uid, created_at DESC, id DESC);
