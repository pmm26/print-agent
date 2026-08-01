# Codebase Review

Reviewed: 2026-08-01

> Remediation update: the findings below are the original audit snapshot.
> The implementation following this review addresses the print-safety,
> identity/idempotency, lifecycle, validation, API/auth, retention,
> permissions, diagnostics, and CI items. The intentionally accepted local
> administration trust model (L6) remains documented in the README; stronger
> local admin sessions are deferred. Real-printer/platform hardware validation
> is still required before production rollout.

## Scope and verification

This review covers the Go service, SQLite schema and repositories, printer workers and transports, macOS/Linux platform drivers, HTTP API/authentication, embedded dashboard, POS simulator, tests, and project configuration. At audit time the working tree already contained uncommitted Linux/BlueZ work; the remediation implementation subsequently extended and tested that work in place.

Verification after remediation:

- Full static inspection of the repository.
- `gofmt -l .`: clean.
- `go test ./...`: passed.
- `go vet ./...`: passed.
- `go test -race ./...`: passed after fixing a race in mock failure injection.
- `GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build ./...`: passed.
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`: passed.
- `node --check internal/webui/web/app.js`: passed.
- `git diff --check` and conflict-marker scan: passed.
- `govulncheck ./...`: no vulnerabilities found; the same scan is configured in CI.

## Executive summary

The architecture has several good foundations: the listener is loopback-only, job insertion is transactional, queue claims use an atomic `UPDATE ... RETURNING`, request sizes are bounded, bearer tokens are stored as hashes, platform code is isolated, and ambiguous timed-out writes normally become `uncertain` instead of being retried.

The highest-priority improvements are in the same core promise the project is designed around: avoiding lost or duplicate receipts. A shutdown can currently turn an ambiguous in-flight write into an automatic retry; database errors during state transitions are ignored; a reprint can be requested while the original is still queued or printing; and deleting a printer can strand its queue. Structured text can also contain ESC/POS control bytes, defeating the stated prohibition on raw commands. The macOS link check fails open when system state cannot be read, which can incorrectly classify buffered bytes as transmitted.

Recommended order:

1. Fix ambiguous-write classification and make persisted delivery transitions mandatory.
2. Sanitize printer text and constrain reprints/deletion.
3. Make link verification distinguish connected, disconnected, and unknown.
4. Harden idempotency, authentication origin binding, and local data permissions.
5. Address lifecycle/concurrency issues, then expand tests and CI.

## High-severity findings

### H1. Context cancellation can cause an ambiguous partial print to be retried automatically

Evidence: `internal/transport/serial.go:155-158`, `internal/platform/linux/rfcomm.go:115-118`, and `internal/printers/worker.go:294-311`.

Both transports close an in-flight writer when the context is cancelled and return `ctx.Err()` with zero bytes for the current chunk. The write may already have handed some or all of that chunk to the OS. The worker only treats a positive byte count or `ErrWriteTimeout` as uncertain. If shutdown happens during the first chunk, it therefore requeues the delivery as a clean failure. On restart it can print again, even though the original may already have printed.

Improve by treating cancellation after a write has started as ambiguous, introducing an explicit `MayHaveWritten`/unknown outcome in `WriteError`, or mapping all interrupted in-flight writes to `ErrWriteTimeout`/`uncertain`. Add a test where cancellation races with a writer that accepts bytes before returning.

### H2. Delivery state persistence errors are ignored after rendering and transmission

Evidence: `internal/printers/worker.go:258`, `internal/printers/worker.go:275`, `internal/printers/worker.go:284`, and `internal/printers/worker.go:307-313`.

Every call to `MarkFailed`, `MarkUncertain`, `MarkTransmitted`, and `Requeue` discards its error. A disk-full, database, or shutdown error can leave a row in `processing` while the worker publishes a success event and moves on. After restart, recovery changes that row to `uncertain`, encouraging an operator reprint of a receipt that may have printed successfully. A failed requeue can similarly leave the queue stuck until restart.

Improve by checking every transition result before publishing the corresponding event or changing worker state. On a persistence failure after bytes were sent, stop that printer worker, emit/log a distinct fatal persistence error through a path that does not depend on the failed database, and never continue claiming work. Repository transitions should also constrain the expected prior state and verify `RowsAffected()`.

### H3. User text can inject arbitrary ESC/POS control commands

Evidence: `internal/escpos/encoding.go:43-57`, `internal/escpos/builder.go:66-72`, and the user-controlled fields rendered throughout `internal/escpos/templates.go:132-232`.

`encodeText` encodes ASCII control characters unchanged. JSON strings can contain `\u001b`, `\u001d`, `\u0010`, NUL, and following command bytes. An authenticated POS can therefore embed ESC/POS instructions in a store name, item, note, footer, or other nominally textual field. This bypasses the README/API claim that raw ESC/POS bytes are not exposed and can trigger cuts, drawer pulses, formatting changes, or device-specific commands.

Improve by sanitizing C0/C1 controls before encoding. Preserve only controls intentionally interpreted by the builder (normally newline, which should be split before encoding), and replace or reject ESC, GS, DLE, NUL, and other non-printing bytes. Add tests that assert no control bytes from payload strings reach the output.

### H4. Reusing a job ID with different content silently drops the new request

Evidence: `internal/jobs/service.go:84-94` returns an existing job based only on `jobId`; `internal/integration/agent_test.go:222-242` even treats a retry with different payload data as a valid duplicate.

Idempotency is safe only when the key identifies the same operation. If the POS accidentally reuses an order ID while changing the printer, delivery set, template, or payload, the API returns `200 duplicate` and silently ignores the new documents. This can lose kitchen/bar tickets without surfacing a conflict.

Improve by storing a canonical request hash with the job. Return the existing result only when the hash matches; otherwise return `409 Conflict` describing an idempotency-key reuse. Include document order/canonical JSON rules in the contract and tests.

### H5. Deleting a printer can permanently strand queued deliveries

Evidence: `internal/printers/manager.go:148-155` deletes the configuration without checking queue state, while workers are only created for configured printers in `internal/printers/manager.go:62-77`.

Queued deliveries retain only the printer ID string. After configuration deletion, no worker can claim them, new jobs reject the ID, and the printer no longer appears in status cards. The rows remain pending indefinitely. Deletion can also race with an active print because the configuration is deleted before the worker is stopped.

Improve by stopping/quiescing the worker first, then transactionally refusing deletion while queued/processing deliveries exist. Offer explicit choices to disable the printer, cancel pending work, or migrate deliveries before deletion. Add an index/query and API tests for each state.

### H6. Link verification fails open when platform state is unknown

Evidence: `internal/platform/darwin/driver.go:50-76` and `internal/platform/darwin/driver.go:146-191`. Linux has the same fail-open policy at `internal/platform/linux/driver.go:148-175`.

On macOS, a `system_profiler` execution/JSON failure becomes an empty device list, and an unknown device is reported as connected. After a serial write succeeds, the worker uses this result to mark the delivery transmitted. This is exactly when the project needs conservative behavior because macOS may buffer writes while the printer is off. A temporary platform-query failure can therefore turn an unknown outcome into false success. Linux also returns success when BlueZ state cannot be queried, though its owned RFCOMM socket provides stronger evidence in the normal path.

Improve the driver contract to return a tri-state result: connected, disconnected, or unknown/error. Before claiming work, unknown may be retried according to policy; after bytes have been accepted, unknown must produce `uncertain`, not `transmitted`. Preserve the underlying platform error for diagnostics.

### H7. The Linux connection timeout does not bound the D-Bus connection call

Evidence: `internal/platform/linux/profile.go:117-138`.

`connectProfile` is called synchronously before the 30-second timer is created. Worker contexts have no deadline, so a stuck BlueZ/D-Bus call can block forever and the configured `connectTimeout` never starts. Similar background-context calls during registration and device enumeration also lack local deadlines.

Improve by deriving a timeout context before calling `connectProfile` (and other external D-Bus operations), or issuing the call asynchronously and selecting over result, cancellation, and timer. Ensure timeout cleanup cannot leak a late RFCOMM descriptor. Add a fake backend that blocks forever and verify `Connect` returns within the configured limit.

### H8. Queue payloads and logs may be readable by other local users

Evidence: `internal/app/service.go:82` and `internal/app/service.go:142-157` create data/log directories with mode `0755`; `internal/storage/sqlite.go:23-40` relies on SQLite/process defaults for database file permissions.

The database stores complete receipt payloads, printer identifiers/endpoints, origins, and token hashes. With a typical umask, newly created SQLite and log files may be `0644`, and the containing directories are traversable. On multi-user machines, another account may be able to read order/customer data.

Improve by creating the data and log directories as `0700`, enforcing `0600` on the database, WAL/SHM files, and log, and documenting the local-data threat model. Existing installations need a one-time permission repair. Consider an optional OS keychain/encryption strategy if payload sensitivity requires protection at rest.

## Medium-severity findings

### M1. Reprint creation is unsafe by state and not atomic

Evidence: `internal/jobs/service.go:194-227` and `internal/jobs/repository.go:245-250`.

The service allows reprinting any delivery, including `queued` or `processing`; the dashboard hides that button, but the API does not enforce it. This can intentionally create two live copies of the same ticket. Reprint numbering performs `COUNT` followed by `INSERT` outside a transaction, so concurrent requests choose the same suffix and one fails. Resolving the original is a separate operation and its error is ignored.

Improve by allowing reprint only from explicitly approved terminal states, performing ID allocation/insertion/original resolution in one transaction, and returning `409` for an invalid current state. A random internal ID is already present; consider a database sequence or retryable unique suffix for the external reprint ID.

### M2. Updating a printer resets omitted fields and re-enables it

Evidence: `internal/api/handlers.go:104-130` and `internal/api/handlers.go:150-165`; the dashboard sends only a subset at `internal/webui/web/app.js:138-151`.

`decodePrinter` makes omitted `enabled` and `autoReconnect` values `true` for both create and update. It also starts from a zero-value config and applies defaults. Configuring the name/encoding of a disabled printer therefore re-enables it, and hidden serial fields can reset to defaults.

Improve by separating create and update DTOs. For update, load the existing record and merge only supplied fields (PATCH semantics), or require a complete PUT and make the dashboard send the complete object. Add regression tests for disabled/manual-reconnect and non-default serial settings.

### M3. Payloads are accepted before template/schema validation

Evidence: `internal/jobs/service.go:146-179` validates only IDs, template name, printer existence, and byte length; JSON is interpreted later in `internal/escpos/templates.go:132-215`.

An omitted `data` field, missing required business fields, wrong types, empty item list, negative prices, or otherwise unusable content is committed and acknowledged as accepted. The asynchronous worker later marks it failed, even though no retry can fix it. Most missing fields currently render as blank receipts rather than errors.

Improve by giving each template a decode-and-validate function and running it before the insert transaction. Validate required order number/items, sensible quantity/price ranges, maximum field/array sizes, and semantic totals as appropriate. Reuse the exact validated model for rendering to avoid divergent rules.

### M4. Tokens are not bound to the origin stored with them

Evidence: the origin is persisted at `internal/api/auth.go:67-69`, but `ValidateToken` at `internal/api/auth.go:81-95` checks only the hash/revocation state; middleware at `internal/api/middleware.go:64-77` does not pass the request origin.

Changing the globally allowed origin makes every old active token usable from the new origin. A token paired by a no-Origin local client is also usable from whichever browser origin is configured later. The stored `allowed_origin` field is therefore metadata rather than an authorization control.

Improve by validating token hash, revocation state, and exact token origin together. Decide explicitly whether changing the configured origin revokes incompatible tokens. Reject browser pairing without an Origin unless a separate local-client token type is intended.

### M5. Pairing/origin validation is too permissive

Evidence: `internal/api/handlers.go:365-377` stores any string as the allowed origin; `internal/api/middleware.go:31-58` compares it verbatim; pairing uses the shared 20-request/second limiter from `internal/api/server.go:46` and `internal/api/server.go:65`.

Values with paths, credentials, whitespace, unsupported schemes, or the special `null` origin are accepted. Allowing `null` grants all opaque/sandboxed/file origins the same CORS identity. A six-digit pairing code valid for five minutes can receive roughly 6,000 guesses under the shared limiter (about a 0.6% success chance per generated code), with no per-code failure cap or cooldown.

Improve by parsing and canonicalizing a strict HTTP(S) origin with scheme plus host/port and no path/query/userinfo; reject `null`. Add a much tighter pairing-specific limiter, invalidate/cool down after a small number of failures, and audit the source. Keep normal status/job traffic on a separate limiter so it cannot starve pairing or vice versa.

### M6. Retention behavior contradicts the stated terminal-delivery policy

Evidence: `DeliveryStatus.Terminal` includes `uncertain` at `internal/jobs/models.go:24-30`, while `PurgeOlderThan` deletes only transmitted, cancelled, and failed rows at `internal/jobs/repository.go:316-336`.

Resolved uncertain deliveries are never purged and can grow forever. Conversely, unresolved failed deliveries are purged, removing items still requiring operator attention. Age is based on creation time, so an old delivery can be deleted immediately after it finally completes. Negative `--retention-days` values are accepted and create a future cutoff that can purge all matching rows on startup. Deleted pages also do not release SQLite file space without vacuum/incremental-vacuum policy.

Improve by defining retention from terminal/resolved timestamps, preserving unresolved attention items, including resolved uncertain rows, validating retention as positive (or defining an explicit disabled value), and adding database-space maintenance.

### M7. Audit and diagnostics failures are silently discarded

Evidence: `internal/diagnostics/diagnostics.go:36-42` ignores event-insert errors; `internal/api/handlers.go:300-326` ignores status/candidate/settings/token/export errors; `internal/api/auth.go:93-94` ignores last-used update errors.

The UI/export can look healthy while portions are missing, the support ZIP can be corrupt, and security/queue audit events can disappear without any warning. Since event persistence is synchronous and uses the same single SQLite connection, failures also need careful handling to avoid recursion.

Improve by returning/logging errors through a non-database fallback logger, collecting partial diagnostics errors in the export manifest, and handling ZIP failures before/while streaming as far as HTTP permits. Add failure-injection tests.

### M8. Token validation writes SQLite on every authenticated request

Evidence: `internal/api/auth.go:81-95`, with browser status polling recommended every 2-3 seconds in the README. The schema at `internal/storage/migrations/0001_init.sql:60-68` has no unique/indexed `token_hash`.

Every poll performs a full token-hash lookup and a write to `last_used_at`, serializing with all queue work on the database's single connection and causing unnecessary WAL churn. Revoked tokens are never retained/purged by policy.

Improve by adding a unique index on `token_hash`, throttling last-used persistence (for example once per several minutes per token), and defining revoked-token retention.

### M9. Manager worker publication/start/stop is race-prone

Evidence: `internal/printers/manager.go:94-110` places a worker in the map before `start` assigns its cancel function; `ApplyPrinter` and `RemovePrinter` perform multi-step operations at `internal/printers/manager.go:133-155`.

Concurrent configuration requests can remove a just-published but not-yet-started worker, after which it starts orphaned. Concurrent updates for the same printer can also overwrite the map entry while an earlier worker remains running. `startWorker` can receive a nil manager context if used before `Start`. These paths are reachable through concurrent local API calls even if the dashboard normally sends one at a time.

Improve by serializing lifecycle changes per printer, constructing/starting before safe publication (with rollback), making worker start/stop idempotent under a mutex/state machine, and rejecting operations before manager startup. Run the suite under `go test -race`.

### M10. Serial write watchdogs assume `Close` always unblocks `Write`

Evidence: `internal/transport/serial.go:126-159`; the RFCOMM equivalent is `internal/platform/linux/rfcomm.go:93-123`.

After timeout/cancellation the code closes the descriptor and then waits unconditionally for the writer goroutine. If a driver/library does not unblock a blocked write on close, the watchdog itself hangs forever and worker shutdown can block. The current test uses a fake whose close deliberately unblocks, so it does not prove behavior on each supported OS/driver.

Improve by using native write deadlines where available, adding a bounded reap path, and hardware/OS-specific tests. If a goroutine must be abandoned, ensure it cannot access reused state and record the leak for diagnostics.

### M11. Receipt rendering has numeric and width edge cases

Evidence: `internal/escpos/templates.go:108-123` and `internal/escpos/templates.go:148-158`; `TwoColumns` always uses normal width at `internal/escpos/builder.go:84-105`.

The double-size TOTAL row calls `TwoColumns`, which lays out using the full normal character width, so it can exceed the physical line width. Quantity-times-price and negating `math.MinInt64` can overflow and print incorrect money values. Non-positive quantities silently become one, potentially hiding bad POS data.

Improve by making column layout aware of current text scale (or use half width explicitly), performing checked arithmetic, and rejecting invalid quantities/prices at acceptance. Add rendered-line-width and numeric-boundary tests.

### M12. The HTTP server and JSON boundary need stricter failure handling

Evidence: `internal/app/service.go:135-138` configures only `ReadHeaderTimeout`; decoders at `internal/api/handlers.go:20`, `internal/api/handlers.go:77`, `internal/api/handlers.go:114`, and `internal/api/handlers.go:369` decode only once.

There is no read/body timeout, write timeout, idle timeout, or maximum header size override. Loopback reduces exposure but a faulty/malicious local process can hold many connections. JSON handlers accept trailing JSON values and unknown fields, making client mistakes easy to miss. Internal error text is returned directly from `writeError` (`internal/api/server.go:130-142`), potentially exposing database/platform details.

Improve by setting bounded server timeouts/header size, using strict decode helpers (`DisallowUnknownFields`, exactly one JSON value, content-type checks), and returning stable public error codes while logging internal detail.

## Lower-severity and maintainability findings

### L1. Cryptographic randomness errors are ignored

Evidence: `internal/api/auth.go:34-36`, `internal/api/auth.go:60-63`, and `internal/jobs/models.go:94-98`.

`crypto/rand` failure is rare but must not produce a nil dereference, predictable token, or all-zero/repeated internal ID. Return errors from code/token/ID generation and fail closed. Add injectable entropy for deterministic failure tests.

### L2. Printer configuration validation is incomplete

Evidence: `internal/config/config.go:86-107`.

Negative/unsupported baud rates, data bits, stop bits, paper widths, oversized display names/endpoints, and mismatched Linux endpoint/device addresses are accepted. The serial transport silently maps every stop-bit value except 2 to one stop bit. Validate all fields, normalize encoding/MAC/endpoint values once, and reject inconsistent combinations.

### L3. `statusProbeEnabled` and `paperWidthMm` are stored but do not affect operation

Evidence: the fields are defined at `internal/config/config.go:43-47` and persisted, but searches find no operational reads. The README/tooling implies status probing can be enabled after hardware validation, yet workers never call `Probe`; paper width does not select layout.

Either implement these features, mark them explicitly reserved/unsupported in the API/UI, or remove them until supported to avoid misleading configuration.

### L4. Timestamp parsing silently converts corrupt data to year zero

Evidence: `internal/jobs/repository.go:23-33` and `internal/config/repository.go:31-33`.

Parse errors are discarded. Corrupt or manually migrated timestamps silently appear as zero values and can affect ordering/status/retention interpretation. Return scan/parse errors with the field and row identity.

### L5. POS simulator contains an HTML-injection sink

Evidence: `scripts/pos-sim/index.html:61-65` and `scripts/pos-sim/index.html:78-84`.

Agent response/error strings and user-derived IDs are inserted with `innerHTML`. Because the simulator stores the bearer token in `localStorage`, an injected handler can steal it. Use `textContent` and create the colored label as a DOM node. Treat this script as development-only and never deploy it on a shared origin.

### L6. Local administration trusts every localhost browser origin

Evidence: `internal/api/middleware.go:16-23` and `internal/api/middleware.go:80-93`.

Any page served from any `localhost`/`127.0.0.1` port is considered the trusted dashboard and can call unauthenticated management endpoints. A local process can already call the loopback API without an Origin, so this may match the intended local-user threat model, but it should be explicit. If hostile local web applications are in scope, issue an admin session secret to the embedded dashboard and validate `Host`, `Origin`, and CSRF tokens more narrowly.

### L7. Migration and recovery tooling is minimal

Evidence: `internal/storage/sqlite.go:43-82` records only migration names.

Applied migrations have no checksum, so editing an old migration is silently ignored on existing installations. There is no startup integrity check, backup/restore command, or documented recovery path for a corrupt queue. Treat migrations as immutable, optionally record checksums, and provide safe backup/integrity diagnostics before schema upgrades.

### L8. Service resource cleanup is incomplete on startup failures

Evidence: `internal/app/service.go:91-139` and `internal/app/service.go:162-173`.

If `Run` cannot bind the port or manager startup fails, the opened database is not closed in every path, and there is no public `Close` for a service constructed but never run. The CLI exits immediately, but library/tests or future embedding can leak resources. Add idempotent service cleanup and use it on all error paths.

### L9. API semantics could be clearer

- `Idempotency-Key` is described as required but is optional in `internal/api/handlers.go:24-29`.
- Invalid delivery status filters return an empty list rather than a validation error (`internal/api/handlers.go:248-257`).
- Create/update responses return the input config with zero `createdAt`/`updatedAt` instead of reloading the persisted record (`internal/api/handlers.go:133-165`).
- Resolve has no corresponding audit event (`internal/jobs/service.go:243-252`).
- Shared rate limiting is global rather than per client/origin, so one busy caller can throttle all POS clients (`internal/api/middleware.go:95-128`).

Document or tighten these behaviors and add API contract tests.

### L10. Dependency and CI assurance is missing from the repository

There is no visible CI workflow, formatter/vet/race gate, dependency vulnerability scan, coverage threshold, or cross-platform compile matrix. This matters especially because platform build tags hide macOS/Windows code on Linux and the current Linux work is uncommitted. Add CI for:

- `gofmt -d`, `go vet ./...`, and `go test -race ./...` on Linux.
- Compile/tests on macOS and Windows (even if Windows remains a scaffold).
- `govulncheck ./...` and dependency update automation.
- JavaScript syntax/lint checks.
- A clean-tree/generated-artifact check.

## Important missing tests

The existing suite covers many happy paths and transport failures, but the following cases should be added before relying on exactly-once behavior:

1. Context cancellation after a chunk may have been partially accepted must become `uncertain` and never auto-retry.
2. Every repository transition failing (disk full/closed DB/injected error) must stop processing and must not publish false success.
3. Same `jobId` plus different payload/printer/documents must return conflict.
4. Reprinting queued/processing work must be rejected; concurrent reprints must be deterministic.
5. Deleting/disabling/updating a printer with queued and processing deliveries.
6. ESC/GS/DLE/NUL characters in every user text field must not reach printer output.
7. macOS/BlueZ connection-state lookup returning unknown after a successful write.
8. Linux `connectProfile` blocking longer than its configured timeout and late-descriptor races.
9. Concurrent create/update/delete/reconnect calls under the race detector.
10. Printer update preserves disabled, manual-reconnect, and non-default serial settings.
11. Retention across transmitted, cancelled, unresolved/resolved failed, unresolved/resolved uncertain, and negative/zero configuration.
12. Strict JSON decoding, invalid origins including `null`, token-origin mismatch, pairing brute-force lockout, and body timeout behavior.
13. Database/log permission checks on supported operating systems.
14. Double-size total width, integer boundaries, invalid quantities/prices, and template-required fields.

## Suggested remediation phases

### Phase 1: print-safety invariants

- Make write outcomes explicitly `not sent`, `sent`, or `ambiguous`.
- Check/guard all persisted state transitions.
- Reject unsafe reprint states and printer deletion with active work.
- Sanitize all text before encoding to ESC/POS.
- Store and verify an idempotency request hash.

### Phase 2: platform and lifecycle reliability

- Add tri-state link verification and bounded D-Bus calls.
- Serialize worker lifecycle operations.
- Correct retention behavior and validate options.
- Harden filesystem permissions and cleanup paths.

### Phase 3: API/security/performance

- Bind tokens to validated origins and harden pairing.
- Strictly decode/validate API models and template data.
- Add server timeouts, token indexes, and throttled last-used updates.
- Make audit/diagnostics failures visible.

### Phase 4: maintainability

- Resolve unused configuration fields.
- Add immutable/checksummed migration practices and recovery documentation.
- Establish cross-platform CI, race testing, vulnerability scanning, and coverage for the cases above.
