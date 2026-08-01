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
    characters_per_line  INTEGER NOT NULL DEFAULT 32,
    encoding             TEXT NOT NULL DEFAULT 'CP858',
    auto_reconnect       INTEGER NOT NULL DEFAULT 1,
    retired_at           TEXT,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);

CREATE TABLE client_tokens (
    id             TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    label          TEXT,
    allowed_origin TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    last_used_at   TEXT,
    revoked_at     TEXT
);

CREATE TABLE jobs (
    uid          TEXT PRIMARY KEY,
    job_id       TEXT NOT NULL,
    template     TEXT NOT NULL,
    data_json    TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    source       TEXT NOT NULL,
    owner_origin TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    UNIQUE (job_id, template)
);
CREATE INDEX idx_jobs_external_created ON jobs(job_id, created_at DESC, uid DESC);
CREATE INDEX idx_jobs_owner_created ON jobs(owner_origin, created_at DESC, uid DESC);

CREATE TRIGGER jobs_immutable
BEFORE UPDATE OF job_id, template, data_json, request_hash, source, owner_origin, created_at ON jobs
BEGIN
    SELECT RAISE(ABORT, 'accepted jobs are immutable');
END;

CREATE TABLE job_printers (
    job_uid              TEXT NOT NULL REFERENCES jobs(uid) ON DELETE CASCADE,
    printer_id           TEXT NOT NULL REFERENCES printers(id) ON DELETE RESTRICT,
    target_order         INTEGER NOT NULL CHECK (target_order >= 0),
    encoding             TEXT NOT NULL,
    characters_per_line  INTEGER NOT NULL CHECK (characters_per_line BETWEEN 8 AND 96),
    accepted_content_hash TEXT NOT NULL,
    cancelled_at         TEXT,
    cancellation_reason  TEXT,
    PRIMARY KEY (job_uid, printer_id),
    UNIQUE (job_uid, target_order)
);
CREATE INDEX idx_job_printers_printer ON job_printers(printer_id, job_uid);

CREATE TRIGGER job_printers_rendering_immutable
BEFORE UPDATE OF printer_id, target_order, encoding, characters_per_line, accepted_content_hash ON job_printers
BEGIN
    SELECT RAISE(ABORT, 'accepted job printer settings are immutable');
END;

CREATE TABLE print_runs (
    queue_sequence       INTEGER PRIMARY KEY AUTOINCREMENT,
    uid                  TEXT NOT NULL UNIQUE,
    job_uid              TEXT NOT NULL,
    printer_id           TEXT NOT NULL,
    run_number           INTEGER NOT NULL CHECK (run_number >= 1),
    trigger              TEXT NOT NULL CHECK (trigger IN ('initial', 'automatic_retry', 'manual_reprint')),
    content_mode         TEXT NOT NULL CHECK (content_mode IN ('normal', 'reprint')),
    previous_run_uid     TEXT,
    reprint_of_run_uid   TEXT,
    reprint_request_id   TEXT,
    reprint_reason       TEXT,
    expected_content_hash TEXT NOT NULL,
    status               TEXT NOT NULL CHECK (status IN ('queued', 'processing', 'transmitted', 'failed', 'uncertain', 'cancelled')),
    retryable            INTEGER NOT NULL DEFAULT 0 CHECK (retryable IN (0, 1)),
    resolution           TEXT CHECK (resolution IS NULL OR resolution IN ('confirmed_printed', 'reprint_requested')),
    error_code           TEXT,
    error_message        TEXT,
    bytes_accepted       INTEGER NOT NULL DEFAULT 0 CHECK (bytes_accepted >= 0),
    created_at           TEXT NOT NULL,
    started_at           TEXT,
    finished_at          TEXT,
    transmitted_at       TEXT,
    resolved_at          TEXT,
    UNIQUE (job_uid, printer_id, run_number),
    UNIQUE (uid, job_uid, printer_id),
    FOREIGN KEY (job_uid, printer_id) REFERENCES job_printers(job_uid, printer_id) ON DELETE CASCADE,
    FOREIGN KEY (previous_run_uid, job_uid, printer_id)
        REFERENCES print_runs(uid, job_uid, printer_id) ON DELETE CASCADE,
    FOREIGN KEY (reprint_of_run_uid, job_uid, printer_id)
        REFERENCES print_runs(uid, job_uid, printer_id) ON DELETE CASCADE,
    CHECK (
        (trigger = 'initial' AND run_number = 1 AND content_mode = 'normal' AND previous_run_uid IS NULL AND reprint_of_run_uid IS NULL AND reprint_request_id IS NULL) OR
        (trigger = 'automatic_retry' AND run_number > 1 AND previous_run_uid IS NOT NULL AND reprint_of_run_uid IS NULL AND reprint_request_id IS NULL) OR
        (trigger = 'manual_reprint' AND run_number > 1 AND content_mode = 'reprint' AND previous_run_uid IS NULL AND reprint_of_run_uid IS NOT NULL AND reprint_request_id IS NOT NULL)
    ),
    CHECK (retryable = 0 OR status = 'failed'),
    CHECK (resolution IS NULL OR status IN ('failed', 'uncertain'))
);
CREATE UNIQUE INDEX idx_runs_one_initial ON print_runs(job_uid, printer_id) WHERE trigger = 'initial';
CREATE UNIQUE INDEX idx_runs_one_retry_child ON print_runs(previous_run_uid) WHERE trigger = 'automatic_retry';
CREATE UNIQUE INDEX idx_runs_one_active ON print_runs(job_uid, printer_id) WHERE status IN ('queued', 'processing');
CREATE UNIQUE INDEX idx_runs_reprint_request_printer ON print_runs(job_uid, printer_id, reprint_request_id) WHERE reprint_request_id IS NOT NULL;
CREATE INDEX idx_runs_queue ON print_runs(printer_id, status, queue_sequence);
CREATE INDEX idx_runs_job_history ON print_runs(job_uid, printer_id, run_number);
CREATE INDEX idx_runs_retry_pending ON print_runs(printer_id, finished_at, queue_sequence)
    WHERE status = 'failed' AND retryable = 1 AND resolution IS NULL;
CREATE INDEX idx_runs_attention ON print_runs(printer_id, status, resolved_at, finished_at);

CREATE TRIGGER print_runs_identity_immutable
BEFORE UPDATE OF uid, job_uid, printer_id, run_number, trigger, content_mode,
    previous_run_uid, reprint_of_run_uid, reprint_request_id, reprint_reason,
    expected_content_hash, created_at ON print_runs
BEGIN
    SELECT RAISE(ABORT, 'Print Run identity and content are immutable');
END;

CREATE TRIGGER print_runs_valid_status_transition
BEFORE UPDATE OF status ON print_runs
WHEN NOT (
    (OLD.status = 'queued' AND NEW.status IN ('processing', 'cancelled')) OR
    (OLD.status = 'processing' AND NEW.status IN ('transmitted', 'failed', 'uncertain')) OR
    OLD.status = NEW.status
)
BEGIN
    SELECT RAISE(ABORT, 'invalid Print Run status transition');
END;

CREATE TABLE request_deduplication (
    kind         TEXT NOT NULL,
    scope_uid    TEXT NOT NULL REFERENCES jobs(uid) ON DELETE CASCADE,
    request_id   TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (kind, scope_uid, request_id)
);

CREATE TABLE print_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_uid      TEXT,
    printer_id   TEXT,
    event_type   TEXT NOT NULL,
    message      TEXT,
    details_json TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX idx_events_created ON print_events(created_at);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
