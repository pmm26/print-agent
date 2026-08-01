# print-agent

Cross-platform Go print agent for Bluetooth ESC/POS receipt printers (58 mm).
A hosted POS webpage submits structured, idempotent print jobs to a loopback
HTTP API; the agent owns all printer logic — queues, reconnects, rendering,
duplicate prevention — and keeps working with no browser open and no internet.

Current platform support: **macOS** (developed and validated first).
Windows (COM ports) and Linux (BlueZ/RFCOMM) adapters are planned; the
transport/connector interfaces already isolate them (`internal/transport`,
`internal/bluetooth`).

## Run

```sh
go run ./cmd/print-agent            # dashboard at http://127.0.0.1:17432/admin
go run ./cmd/print-agent --port 17555 --data-dir /tmp/agent-data
```

Data (SQLite DB, rolling logs) lives in `~/Library/Application Support/print-agent/`
by default.

## Hardware validation (btprobe)

Run these before trusting a new printer model:

```sh
go run ./cmd/btprobe list                              # candidate endpoints + paired devices
go run ./cmd/btprobe test /dev/cu.MyPrinter            # formatted test page
go run ./cmd/btprobe charset /dev/cu.MyPrinter         # find the right Spanish code page
go run ./cmd/btprobe multi /dev/cu.P1 /dev/cu.P2 ...   # concurrent prints, several printers
go run ./cmd/btprobe status /dev/cu.MyPrinter          # DLE EOT real-time status support
```

## Printer setup (v1 flow)

1. Pair the printer in the OS Bluetooth settings (dashboard has a shortcut).
2. Dashboard → **Add printer** → pick the endpoint, assign an ID (`cashier`,
   `kitchen`, `bar`), choose encoding (CP858 covers Spanish + €).
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

3. Submit jobs — one master job, one delivery per printer, stable IDs:

```js
await fetch("http://127.0.0.1:17432/api/v1/jobs", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "Authorization": `Bearer ${token}`,
    "Idempotency-Key": job.jobId,          // must equal jobId
  },
  body: JSON.stringify({
    jobId: "store-001:order-1256",
    documents: [
      { deliveryId: "store-001:order-1256:cashier", printerId: "cashier",
        template: "customer-receipt", data: { orderNumber: "1256", items: [/*…*/], totalCents: 92000 } },
      { deliveryId: "store-001:order-1256:kitchen", printerId: "kitchen",
        template: "kitchen-ticket", data: { orderNumber: "1256", items: [/*…*/] } },
    ],
  }),
});
```

Resubmitting the same `jobId` returns the existing status and never prints a
duplicate. Poll `GET /api/v1/status` (2–3 s) while the POS page is open.

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

## Delivery states

`queued → processing → transmitted` — *transmitted* means all bytes reached
the OS/Bluetooth link, not that paper came out (cheap printers give no
acknowledgement). Failures classify as:

- `failed` — nothing meaningful was sent; clean failures retry automatically
  (max 5 attempts), then wait for the operator.
- `uncertain` — transmission had begun (or timed out mid-write); **never**
  auto-retried. The dashboard queue offers **Reprint** (adds a `*** REPRINT ***`
  banner), **Mark resolved**, and **Cancel**.

On startup, deliveries stuck in `processing` become `uncertain` (the agent
may have died mid-write). Queued jobs survive restarts — SQLite is the queue.

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
