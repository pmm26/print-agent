CREATE TABLE agent_settings (
    singleton                       INTEGER PRIMARY KEY CHECK (singleton = 1),
    agent_id                        TEXT NOT NULL UNIQUE,
    allowed_pos_origin              TEXT,
    websocket_mode                  TEXT NOT NULL DEFAULT 'disabled'
        CHECK (websocket_mode IN ('disabled', 'server', 'client', 'both')),
    websocket_server_path           TEXT NOT NULL DEFAULT '/api/v2/events/ws',
    websocket_server_bind_address   TEXT NOT NULL DEFAULT '127.0.0.1',
    websocket_allow_non_loopback    INTEGER NOT NULL DEFAULT 0 CHECK (websocket_allow_non_loopback IN (0, 1)),
    websocket_server_auth_required  INTEGER NOT NULL DEFAULT 1 CHECK (websocket_server_auth_required IN (0, 1)),
    websocket_server_tls            INTEGER NOT NULL DEFAULT 0 CHECK (websocket_server_tls IN (0, 1)),
    websocket_tls_cert_path         TEXT,
    websocket_tls_key_ref           TEXT,
    websocket_client_queue_capacity INTEGER NOT NULL DEFAULT 256 CHECK (websocket_client_queue_capacity BETWEEN 1 AND 10000),
    websocket_connection_limit      INTEGER NOT NULL DEFAULT 32 CHECK (websocket_connection_limit BETWEEN 1 AND 1000),
    websocket_heartbeat_ms           INTEGER NOT NULL DEFAULT 20000 CHECK (websocket_heartbeat_ms BETWEEN 1000 AND 300000),
    websocket_write_timeout_ms       INTEGER NOT NULL DEFAULT 10000 CHECK (websocket_write_timeout_ms BETWEEN 1000 AND 120000),
    websocket_max_message_bytes      INTEGER NOT NULL DEFAULT 65536 CHECK (websocket_max_message_bytes BETWEEN 1024 AND 1048576),
    websocket_replay_limit           INTEGER NOT NULL DEFAULT 1000 CHECK (websocket_replay_limit BETWEEN 1 AND 5000),
    event_retention_seconds         INTEGER NOT NULL DEFAULT 604800 CHECK (event_retention_seconds >= 3600),
    max_unacknowledged_age_seconds  INTEGER NOT NULL DEFAULT 604800 CHECK (max_unacknowledged_age_seconds >= 3600),
    dead_letter_retention_seconds   INTEGER NOT NULL DEFAULT 2592000 CHECK (dead_letter_retention_seconds >= 3600),
    event_disk_high_water_bytes     INTEGER NOT NULL DEFAULT 268435456 CHECK (event_disk_high_water_bytes >= 1048576),
    created_at                      TEXT NOT NULL,
    updated_at                      TEXT NOT NULL
);

CREATE TABLE printers (
    id                    TEXT PRIMARY KEY,
    display_name          TEXT NOT NULL,
    enabled               INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    transport             TEXT NOT NULL DEFAULT 'bluetooth-serial'
        CHECK (transport IN ('bluetooth-serial', 'mock')),
    device_address        TEXT,
    endpoint              TEXT,
    connection_preference TEXT NOT NULL DEFAULT 'auto'
        CHECK (connection_preference IN ('auto', 'rfcomm', 'ble')),
    baud_rate             INTEGER NOT NULL DEFAULT 9600 CHECK (baud_rate BETWEEN 300 AND 1000000),
    data_bits             INTEGER NOT NULL DEFAULT 8 CHECK (data_bits BETWEEN 5 AND 8),
    stop_bits             INTEGER NOT NULL DEFAULT 1 CHECK (stop_bits IN (1, 2)),
    parity                TEXT NOT NULL DEFAULT 'none' CHECK (parity IN ('none', 'odd', 'even')),
    characters_per_line   INTEGER NOT NULL DEFAULT 32 CHECK (characters_per_line BETWEEN 8 AND 96),
    encoding              TEXT NOT NULL DEFAULT 'CP858',
    auto_reconnect        INTEGER NOT NULL DEFAULT 1 CHECK (auto_reconnect IN (0, 1)),
    configuration_generation INTEGER NOT NULL DEFAULT 1 CHECK (configuration_generation >= 1),
    retired_at            TEXT,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);
CREATE INDEX idx_printers_active ON printers(enabled, id) WHERE retired_at IS NULL;
CREATE INDEX idx_printers_device ON printers(device_address) WHERE device_address IS NOT NULL;

CREATE TABLE api_credentials (
    id             TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    credential_type TEXT NOT NULL CHECK (credential_type IN ('pos', 'events', 'admin')),
    label          TEXT,
    allowed_origin TEXT,
    created_at     TEXT NOT NULL,
    last_used_at   TEXT,
    revoked_at     TEXT
);

CREATE TABLE credential_scopes (
    credential_id TEXT NOT NULL REFERENCES api_credentials(id) ON DELETE CASCADE,
    scope         TEXT NOT NULL CHECK (scope IN ('jobs:submit', 'jobs:read', 'status:read', 'events:read', 'admin')),
    PRIMARY KEY (credential_id, scope)
);
CREATE INDEX idx_credential_scopes_scope ON credential_scopes(scope, credential_id);

CREATE TABLE websocket_allowed_origins (
    origin     TEXT PRIMARY KEY,
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
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
CREATE INDEX idx_jobs_expiry ON jobs(expires_at);

CREATE TRIGGER jobs_immutable
BEFORE UPDATE OF job_id, template, data_json, request_hash, source, owner_origin, created_at ON jobs
BEGIN
    SELECT RAISE(ABORT, 'accepted jobs are immutable');
END;

CREATE TABLE job_targets (
    job_uid               TEXT NOT NULL REFERENCES jobs(uid) ON DELETE CASCADE,
    printer_id            TEXT NOT NULL REFERENCES printers(id) ON DELETE RESTRICT,
    target_order          INTEGER NOT NULL CHECK (target_order >= 0),
    encoding              TEXT NOT NULL,
    characters_per_line   INTEGER NOT NULL CHECK (characters_per_line BETWEEN 8 AND 96),
    accepted_content_hash TEXT NOT NULL,
    cancelled_at          TEXT,
    cancellation_reason   TEXT,
    PRIMARY KEY (job_uid, printer_id),
    UNIQUE (job_uid, target_order)
);
CREATE INDEX idx_job_targets_printer ON job_targets(printer_id, job_uid);

CREATE TRIGGER job_targets_rendering_immutable
BEFORE UPDATE OF printer_id, target_order, encoding, characters_per_line, accepted_content_hash ON job_targets
BEGIN
    SELECT RAISE(ABORT, 'accepted job target settings are immutable');
END;

CREATE TABLE idempotency_records (
    kind         TEXT NOT NULL CHECK (kind IN ('job_acceptance', 'manual_reprint')),
    scope_uid    TEXT NOT NULL,
    request_key  TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    resource_uid TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    PRIMARY KEY (kind, scope_uid, request_key)
);
CREATE INDEX idx_idempotency_expiry ON idempotency_records(expires_at);

CREATE TABLE manual_reprint_requests (
    uid            TEXT PRIMARY KEY,
    job_uid        TEXT NOT NULL REFERENCES jobs(uid) ON DELETE CASCADE,
    request_id     TEXT NOT NULL,
    request_hash   TEXT NOT NULL,
    reason_code    TEXT,
    operator_note  TEXT,
    created_by     TEXT,
    created_at     TEXT NOT NULL,
    UNIQUE (job_uid, request_id)
);

CREATE TABLE print_runs (
    queue_sequence        INTEGER PRIMARY KEY AUTOINCREMENT,
    uid                   TEXT NOT NULL UNIQUE,
    job_uid               TEXT NOT NULL,
    printer_id            TEXT NOT NULL,
    chain_uid             TEXT NOT NULL,
    run_number            INTEGER NOT NULL CHECK (run_number >= 1),
    attempt_number        INTEGER NOT NULL CHECK (attempt_number >= 1),
    trigger               TEXT NOT NULL CHECK (trigger IN ('initial', 'automatic_retry', 'manual_reprint')),
    content_mode          TEXT NOT NULL CHECK (content_mode IN ('normal', 'reprint')),
    previous_run_uid      TEXT,
    reprint_of_run_uid    TEXT,
    reprint_request_uid   TEXT REFERENCES manual_reprint_requests(uid) ON DELETE RESTRICT,
    reprint_request_id    TEXT,
    reprint_reason        TEXT,
    expected_content_hash TEXT NOT NULL,
    status                TEXT NOT NULL CHECK (status IN ('queued', 'claimed', 'transmitting', 'transmitted', 'failed', 'uncertain', 'cancelled')),
    resolution            TEXT CHECK (resolution IS NULL OR resolution IN ('confirmed_printed', 'reprint_requested')),
    retry_disposition     TEXT NOT NULL DEFAULT 'none'
        CHECK (retry_disposition IN ('none', 'pending_reconnect', 'created', 'exhausted', 'suppressed')),
    error_code            TEXT,
    error_message         TEXT,
    bytes_accepted        INTEGER NOT NULL DEFAULT 0 CHECK (bytes_accepted >= 0),
    created_at            TEXT NOT NULL,
    claimed_at            TEXT,
    transmission_started_at TEXT,
    finished_at           TEXT,
    transmitted_at        TEXT,
    resolved_at           TEXT,
    UNIQUE (job_uid, printer_id, run_number),
    UNIQUE (chain_uid, attempt_number),
    UNIQUE (uid, job_uid, printer_id),
    FOREIGN KEY (job_uid, printer_id) REFERENCES job_targets(job_uid, printer_id) ON DELETE CASCADE,
    FOREIGN KEY (previous_run_uid, job_uid, printer_id)
        REFERENCES print_runs(uid, job_uid, printer_id) ON DELETE RESTRICT,
    FOREIGN KEY (reprint_of_run_uid, job_uid, printer_id)
        REFERENCES print_runs(uid, job_uid, printer_id) ON DELETE RESTRICT,
    CHECK (
        (trigger = 'initial' AND run_number = 1 AND attempt_number = 1 AND content_mode = 'normal'
            AND previous_run_uid IS NULL AND reprint_of_run_uid IS NULL AND reprint_request_id IS NULL) OR
        (trigger = 'automatic_retry' AND run_number > 1 AND attempt_number > 1
            AND previous_run_uid IS NOT NULL AND reprint_of_run_uid IS NULL AND reprint_request_id IS NULL) OR
        (trigger = 'manual_reprint' AND run_number > 1 AND attempt_number = 1 AND content_mode = 'reprint'
            AND previous_run_uid IS NULL AND reprint_of_run_uid IS NOT NULL AND reprint_request_id IS NOT NULL)
    ),
    CHECK (retry_disposition = 'none' OR status = 'failed'),
    CHECK (
        resolution IS NULL OR
        (resolution = 'confirmed_printed' AND status IN ('transmitted', 'uncertain')) OR
        (resolution = 'reprint_requested' AND status IN ('failed', 'uncertain'))
    )
);
CREATE UNIQUE INDEX idx_runs_one_initial ON print_runs(job_uid, printer_id) WHERE trigger = 'initial';
CREATE UNIQUE INDEX idx_runs_one_retry_child ON print_runs(previous_run_uid) WHERE trigger = 'automatic_retry';
CREATE UNIQUE INDEX idx_runs_one_active ON print_runs(job_uid, printer_id)
    WHERE status IN ('queued', 'claimed', 'transmitting');
CREATE UNIQUE INDEX idx_runs_reprint_request_printer ON print_runs(job_uid, printer_id, reprint_request_id)
    WHERE reprint_request_id IS NOT NULL;
CREATE INDEX idx_runs_queue ON print_runs(printer_id, status, queue_sequence);
CREATE INDEX idx_runs_job_history ON print_runs(job_uid, printer_id, run_number);
CREATE INDEX idx_runs_retry_pending ON print_runs(printer_id, queue_sequence)
    WHERE status = 'failed' AND retry_disposition = 'pending_reconnect' AND resolution IS NULL;
CREATE INDEX idx_runs_attention ON print_runs(printer_id, status, resolution, retry_disposition, finished_at);

CREATE TRIGGER print_runs_identity_immutable
BEFORE UPDATE OF uid, job_uid, printer_id, chain_uid, run_number, attempt_number, trigger,
    content_mode, previous_run_uid, reprint_of_run_uid, reprint_request_uid,
    reprint_request_id, reprint_reason, expected_content_hash, created_at ON print_runs
BEGIN
    SELECT RAISE(ABORT, 'Print Run identity and content are immutable');
END;

CREATE TRIGGER print_runs_valid_status_transition
BEFORE UPDATE OF status ON print_runs
WHEN NOT (
    (OLD.status = 'queued' AND NEW.status IN ('claimed', 'cancelled')) OR
    (OLD.status = 'claimed' AND NEW.status IN ('transmitting', 'failed', 'cancelled')) OR
    (OLD.status = 'transmitting' AND NEW.status IN ('transmitted', 'failed', 'uncertain')) OR
    OLD.status = NEW.status
)
BEGIN
    SELECT RAISE(ABORT, 'invalid Print Run status transition');
END;

CREATE TABLE durable_events (
    sequence            INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id            TEXT NOT NULL UNIQUE,
    schema_version      TEXT NOT NULL,
    event_type          TEXT NOT NULL,
    category            TEXT NOT NULL CHECK (category IN ('agent', 'printer', 'job', 'print_run', 'websocket', 'security', 'config')),
    occurred_at         TEXT NOT NULL,
    recorded_at         TEXT NOT NULL,
    agent_id            TEXT NOT NULL,
    publish_scope       TEXT NOT NULL CHECK (publish_scope IN ('external', 'local')),
    durability          TEXT NOT NULL CHECK (durability IN ('domain', 'operational')),
    printer_id          TEXT,
    job_uid             TEXT,
    external_job_id     TEXT,
    template            TEXT,
    run_uid             TEXT,
    correlation_id      TEXT,
    causation_event_id  TEXT,
    error_code          TEXT,
    safe_message        TEXT,
    payload_json        TEXT NOT NULL,
    expires_at          TEXT NOT NULL
);
CREATE INDEX idx_events_scope_sequence ON durable_events(publish_scope, sequence);
CREATE INDEX idx_events_type_sequence ON durable_events(event_type, sequence);
CREATE INDEX idx_events_printer_sequence ON durable_events(printer_id, sequence) WHERE printer_id IS NOT NULL;
CREATE INDEX idx_events_job_sequence ON durable_events(job_uid, sequence) WHERE job_uid IS NOT NULL;
CREATE INDEX idx_events_run_sequence ON durable_events(run_uid, sequence) WHERE run_uid IS NOT NULL;
CREATE INDEX idx_events_expiry ON durable_events(expires_at);

CREATE TABLE websocket_destinations (
    id                    TEXT PRIMARY KEY,
    enabled               INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    endpoint              TEXT NOT NULL,
    auth_type             TEXT NOT NULL DEFAULT 'bearer' CHECK (auth_type IN ('none', 'bearer')),
    secret_ref            TEXT,
    custom_ca_path        TEXT,
    categories_json       TEXT NOT NULL DEFAULT '["agent","printer","job","print_run","config"]',
    connect_timeout_ms    INTEGER NOT NULL DEFAULT 10000 CHECK (connect_timeout_ms BETWEEN 1000 AND 120000),
    heartbeat_interval_ms INTEGER NOT NULL DEFAULT 20000 CHECK (heartbeat_interval_ms BETWEEN 1000 AND 300000),
    stale_timeout_ms      INTEGER NOT NULL DEFAULT 60000 CHECK (stale_timeout_ms BETWEEN 2000 AND 600000),
    write_timeout_ms      INTEGER NOT NULL DEFAULT 10000 CHECK (write_timeout_ms BETWEEN 1000 AND 120000),
    reconnect_min_ms      INTEGER NOT NULL DEFAULT 2000 CHECK (reconnect_min_ms BETWEEN 100 AND 600000),
    reconnect_max_ms      INTEGER NOT NULL DEFAULT 60000 CHECK (reconnect_max_ms BETWEEN 1000 AND 3600000),
    reconnect_jitter      REAL NOT NULL DEFAULT 0.2 CHECK (reconnect_jitter BETWEEN 0 AND 1),
    ack_timeout_ms        INTEGER NOT NULL DEFAULT 30000 CHECK (ack_timeout_ms BETWEEN 1000 AND 600000),
    outbound_queue_capacity INTEGER NOT NULL DEFAULT 512 CHECK (outbound_queue_capacity BETWEEN 1 AND 100000),
    last_connected_at     TEXT,
    last_delivery_at      TEXT,
    last_ack_at           TEXT,
    consecutive_failures  INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    last_error_code       TEXT,
    last_error_message    TEXT,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_one_enabled_destination ON websocket_destinations(enabled) WHERE enabled = 1;

CREATE TABLE event_deliveries (
    destination_id       TEXT NOT NULL REFERENCES websocket_destinations(id) ON DELETE CASCADE,
    event_sequence       INTEGER NOT NULL REFERENCES durable_events(sequence) ON DELETE CASCADE,
    status               TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'inflight', 'acked', 'dead_letter')),
    attempt_count        INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at      TEXT NOT NULL,
    first_attempt_at     TEXT,
    last_attempt_at      TEXT,
    acknowledgement_deadline TEXT,
    acknowledged_at      TEXT,
    last_error_code      TEXT,
    last_error_message   TEXT,
    PRIMARY KEY (destination_id, event_sequence)
);
CREATE INDEX idx_deliveries_pending ON event_deliveries(destination_id, status, next_attempt_at, event_sequence);

CREATE TABLE outbox_attempts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    destination_id  TEXT NOT NULL,
    event_sequence  INTEGER NOT NULL,
    attempt_number  INTEGER NOT NULL,
    started_at      TEXT NOT NULL,
    finished_at     TEXT,
    outcome         TEXT CHECK (outcome IS NULL OR outcome IN ('acked', 'retry', 'dead_letter', 'shutdown')),
    error_code      TEXT,
    error_message   TEXT,
    FOREIGN KEY (destination_id, event_sequence)
        REFERENCES event_deliveries(destination_id, event_sequence) ON DELETE CASCADE
);
CREATE INDEX idx_outbox_attempts_created ON outbox_attempts(started_at DESC, id DESC);

CREATE TABLE dead_letter_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    destination_id  TEXT NOT NULL,
    event_id         TEXT NOT NULL,
    event_sequence   INTEGER NOT NULL,
    event_type       TEXT NOT NULL,
    payload_json     TEXT NOT NULL,
    failed_at        TEXT NOT NULL,
    reason           TEXT NOT NULL,
    error_code       TEXT,
    error_message    TEXT,
    attempt_count    INTEGER NOT NULL,
    expires_at       TEXT NOT NULL,
    UNIQUE (destination_id, event_id)
);
CREATE INDEX idx_dead_letters_expiry ON dead_letter_events(expires_at);

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
