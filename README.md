# print-agent

Cross-platform Go print agent for Bluetooth ESC/POS receipt printers (58 mm).
A hosted POS webpage submits structured, idempotent print jobs to a loopback
HTTP API; the agent owns all printer logic — queues, reconnects, rendering,
duplicate prevention — and keeps working with no browser open and no internet.

Current platform support: **Windows and macOS**.
All OS-specific code lives behind the `platform.Driver` interface in
`internal/platform/` — one subfolder per OS. The core never mentions an
operating system.

## Run

```sh
go run ./cmd/print-agent            # dashboard at http://127.0.0.1:17432/admin
go run ./cmd/print-agent --port 17555 --data-dir /tmp/agent-data
```

Data (SQLite DB, rolling logs) lives in the platform's user data directory by
default. Structured application logs are available under **System → System
Logs** and retained for two hours. Printer and Print Run events are available
under **Operations → Printer Logs** and retained with the existing 48-hour Job
history.

## Hardware validation (btprobe)

Run these before trusting a new printer model:

```sh
go run ./cmd/btprobe list                                  # paired candidate printers
go run ./cmd/btprobe test COM7                             # Windows formatted test page
go run ./cmd/btprobe charset COM7                          # charset page
go run ./cmd/btprobe multi COM7 COM8                       # concurrent repeated prints
go run ./cmd/btprobe status COM7                           # DLE EOT status, when supported
```

On macOS, use the discovered `/dev/cu.*` endpoint in the same commands.
`test`, `charset`, and `multi` use the active platform driver. `status` needs
bidirectional access from the serial library and therefore supports serial
endpoints only.

## Printer setup

1. Pair the printer in the operating system Bluetooth settings. On Windows,
   confirm the device exposes an outgoing serial COM port such as `COM7`.
2. Dashboard → **Printers** → **Add printer** → pick the discovered endpoint,
   assign an ID (`cashier`, `kitchen`, `bar`), and choose an encoding (CP858
   covers Spanish + €).
3. **Test print.**

All three printers may advertise the same Bluetooth name — printers are
stored by endpoint + MAC address; label them physically.

## POS integration

1. Dashboard → **POS Pairing** → set the exact allowed origin
   (e.g. `https://pos.example.com`) → **Generate pairing code**.
2. The POS page exchanges the code for a bearer token (store it per
   installation):

```js
const { token } = await (await fetch("http://127.0.0.1:17432/api/v1/pair", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ code: userEnteredCode }),
})).json();
```

3. Submit a Job — one business/order ID, one template/data payload, and one
   or more original printers:

```js
await fetch("http://127.0.0.1:17432/api/v1/jobs", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "Authorization": `Bearer ${token}`,
  },
  body: JSON.stringify({
    jobId: "store-001:order-1256",
    template: "kitchen-ticket",
    data: { orderNumber: "1256", items: [/*…*/] },
    printerIds: ["kitchen-1", "kitchen-2"],
  }),
});
```

The idempotency identity is `jobId + template`:

- An identical retry returns the existing internal Job `uid` and creates no
  new Print Runs.
- Reusing the identity with different data or printers returns `409 Conflict`.
- A different template under the same `jobId` creates a separate Job.
- Changed order data must use a new `jobId`.
- Printer ordering does not affect equality.

Completed Job data is retained for 48 hours. After it is purged, the same
`jobId + template` can be accepted and printed again by design.

Read one Job with `GET /api/v1/jobs/{uid}` or find related Jobs with
`GET /api/v1/jobs?jobId=order-1256`. Print Runs use
`GET /api/v1/print-runs/{uid}`.

A selected or whole-Job reprint uses `POST /api/v1/jobs/{uid}/reprint`:

```json
{
  "reprintRequestId": "operator-action-789",
  "printerIds": ["kitchen-1", "kitchen-2"],
  "reason": "Tickets were damaged"
}
```

The request ID is idempotent within that Job. Manual reprints target original
printers only and print `*** REPRINT - RUN N ***`.

Templates: `customer-receipt`, `kitchen-ticket`, `bar-ticket`, `test-page`.
There is deliberately no endpoint for raw ESC/POS bytes.

### Browser notes (Chrome/Edge)

Chrome 142+ shows a **Local Network Access** permission prompt the first time
the (HTTPS) POS page calls the loopback API — the user must accept it once;
the grant also relaxes mixed-content blocking for the local request. The
agent answers CORS preflights (including the legacy
`Access-Control-Allow-Private-Network` header) for the configured origin only.
Safari blocks HTTPS→`http://127.0.0.1` entirely; supporting Safari would
require the local-HTTPS mode (not yet implemented).

`scripts/pos-sim/index.html` is a minimal fake POS page for end-to-end
testing of pairing, CORS, the LNA prompt, and idempotent submission.

For local kitchen-ticket testing, dashboard → **Dev** → **POS Simulator** can
submit editable tickets to one or more configured printers, inspect their
Print Runs, verify job idempotency/conflicts, and trigger selected-target
reprints. These actions use the real queue and can produce physical output.

## Print Run states

`queued → processing → transmitted` — *transmitted* means all bytes reached
the OS/Bluetooth link, not that paper came out (cheap printers give no
acknowledgement). Failures classify as:

- A printer that is offline leaves its existing Run queued; connection attempts
  are not Print Runs.
- A confirmed zero-byte execution failure becomes `failed`. After the printer
  reconnects, a distinct `automatic_retry` Print Run is created, up to three
  retries per execution chain.
- `failed` — deterministic rendering/validation failed and operator attention
  is required.
- `uncertain` — transmission had begun (or timed out mid-write); **never**
  auto-retried. An operator must explicitly choose **It printed** or request a
  marked reprint.

On startup, Print Runs stuck in `processing` become `uncertain` (the agent
may have died mid-write). Queued Jobs survive restarts — SQLite is the queue.
If a Print Run state write to SQLite fails, all workers pause new claims and
retry persistence indefinitely. `/api/v1/status` and the dashboard expose this
pause; printing resumes automatically after the database recovers.

Terminal/resolved Jobs and events are retained for 48 hours. Queued,
processing, retry-pending, and unresolved failed/uncertain work is never
purged.

## Local data and pre-release upgrades

The data directory and log directory are forced to owner-only mode (`0700`)
and the SQLite/log files to `0600`. The queue contains receipt payloads and
should be treated as sensitive local data. Authentication protects the POS
browser boundary; management endpoints intentionally trust local machine
access and are not a defense against another process running as the user.

This project is pre-release. The Job/Print Run schema is a destructive rebuild
baseline; old pre-release databases are not migrated. Stop the agent, delete
or relocate its data directory, restart it, and configure printers and POS
pairing again. The agent never silently deletes an old database itself.
Applied migrations are checksummed and startup runs SQLite `quick_check`.

## Development

```sh
go test ./...   # unit + integration (mock transports with scripted failures)
go vet ./...
```

Architecture (one worker per printer, no shared locks on the print path):

```
HTTP handler → JobService → SQLite tx → wake channel → printer worker
                                          worker: claim → render ESC/POS → write (chunked, watchdog)
```
