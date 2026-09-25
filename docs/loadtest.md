# Load test tool

`cmd/loadtest` is a small load generator for the wallet-go API. It ramps the
load up in stages until the API "breaks", then waits for it to recover and
verifies that the ledger is still consistent (no money created or lost).

It is a measuring tool, not part of the service. It only uses the Go standard
library, and it refuses to run against any host other than `localhost`.

## What it does

1. **Setup.** Creates funded accounts through the public API (each one gets a
   deposit of 1,000,000,000 cents).
2. **Ramp-up.** Runs stages of fixed duration. The first stage uses `-start`
   workers and the number doubles at every stage (8, 16, 32, ...) up to `-max`.
   Each worker is a goroutine that sends requests back to back for the whole
   stage.
3. **Stop condition.** After each stage it checks the results. The API counts
   as **broken** when either:
   - the failure ratio is above `-err-limit` (default 5%), or
   - the p99 latency is above `-p99-limit` (default 2s).

   A failure is any response outside `2xx`, plus transport errors (client
   timeout, connection refused, connection reset).
4. **Recovery.** Polls the API until it answers again (up to 60s) and prints
   how long it took.
5. **Consistency check.** Reads the balance of every account it created and
   compares the total with what was deposited. Transfers only move money
   between those accounts, so the sum must not change.

## Prerequisites

- The API running and reachable (see [running-locally.md](running-locally.md)).
- `WALLET_API_KEY` of the API equal to `-key` (default `dev-key`, which matches
  `.env.example`).

## Usage

```bash
go run ./cmd/loadtest [flags]
```

| Flag           | Default                 | Description                                                    |
|----------------|-------------------------|----------------------------------------------------------------|
| `-base`        | `http://localhost:8080` | API base URL. Only `localhost`, `127.0.0.1` and `::1` are accepted. |
| `-key`         | `dev-key`               | Value sent in the `X-API-Key` header.                          |
| `-scenario`    | `hot`                   | `hot`, `spread` or `read` (see below).                         |
| `-stage`       | `10s`                   | Duration of each load stage.                                   |
| `-start`       | `8`                     | Workers in the first stage. Doubles every stage.               |
| `-max`         | `512`                   | Maximum number of workers. The run ends here if nothing broke. |
| `-accounts`    | `100`                   | Accounts used by `spread` and `read`. Ignored by `hot` (always 2). |
| `-err-limit`   | `0.05`                  | Failure ratio (0 to 1) that counts as broken.                  |
| `-p99-limit`   | `2s`                    | p99 latency that counts as broken.                             |

### Scenarios

| Scenario | Traffic | What it stresses |
|----------|---------|------------------|
| `hot`    | Every request is a `POST /transfers` between the **same two accounts**. | Row locking and the wait for a pool connection. This is the high-contention case. |
| `spread` | `POST /transfers` between random pairs out of `-accounts` accounts. | Connection pool, CPU and PostgreSQL when there is little contention. |
| `read`   | `GET /accounts/{id}/balance` on random accounts. | The `SUM` over the entries and the pool, without writes. |

Every transfer moves 1 cent and carries a unique `Idempotency-Key`, so no
request is ever replayed.

### Examples

```bash
# High contention, up to 2048 workers (the default cap of 512 may not be
# enough for the API to break in this scenario)
go run ./cmd/loadtest -scenario hot -max 2048

# Low contention, longer stages for steadier numbers
go run ./cmd/loadtest -scenario spread -stage 20s -max 2048

# Reads only, tighter latency limit
go run ./cmd/loadtest -scenario read -p99-limit 500ms -max 2048

# A single fixed load level instead of a ramp (start equals max)
go run ./cmd/loadtest -scenario hot -start 64 -max 64 -stage 30s

# Tolerate more failures before calling it broken
go run ./cmd/loadtest -scenario spread -err-limit 0.20
```

To run one fixed level, set `-start` and `-max` to the same value: the loop runs
exactly one stage.

## Reading the output

```
Setting up 2 funded accounts (scenario "hot")...
Ramping up: 4 workers, doubling every 2s until failure > 5% or p99 > 2s (max 8 workers)

workers=4    rps=318     fail=  0.0%  p50=12.4ms   p95=14.2ms   p99=16.6ms   max=24.7ms   status=map[201:639] errs=map[]
workers=8    rps=318     fail=  0.0%  p50=24.7ms   p95=27.8ms   p99=33.7ms   max=43.5ms   status=map[201:644] errs=map[]

The API did NOT break up to 8 workers.

Waiting for the API to recover...
Recovered after 2ms.
Consistency check PASSED: balances sum to 2000000000 cents, no money created or lost.
```

Each stage prints one line:

| Field     | Meaning |
|-----------|---------|
| `workers` | Concurrent workers in that stage. |
| `rps`     | Completed requests per second over the whole stage. |
| `fail`    | Share of requests that were not `2xx` or hit a transport error. |
| `p50` `p95` `p99` `max` | Latency percentiles of the stage. |
| `status`  | Count of responses per HTTP status, for example `map[201:639 503:120]`. |
| `errs`    | Count of transport errors by kind: `client_timeout`, `connection_refused`, `connection_reset`, `transport_error`. |

How to interpret typical patterns:

- **`rps` flat while `workers` doubles and latency doubles too.** The system is
  saturated: extra concurrency only lengthens the queue. In the `hot` scenario
  this is the row lock serializing transfers on the same account.
- **`503` statuses.** The API gave up waiting: the pool connection acquire
  timeout (3s) or the per-request timeout (5s) expired. This is the API failing
  fast on purpose instead of hanging.
- **`client_timeout` errors.** The tool itself waited 10s without an answer.
  The API was slower than every timeout it has.
- **`connection_refused` or `connection_reset`.** The server stopped accepting
  or dropped connections, or the process died.
- **Consistency check FAILED.** Money was created or lost under load. That is a
  correctness bug, far more serious than any throughput number. The tool exits
  with a non-zero status in this case.

## Exit status

| Code | Meaning |
|------|---------|
| `0`  | Run finished, API recovered, ledger consistent. Breaking the API is a normal outcome and still exits `0`. |
| `1`  | Setup failed (API not running, wrong key), the API did not recover within 60s, or the consistency check failed. |

## Safety and limits

- **Localhost only.** Any other host is rejected before a single request is sent.
- **Data is left behind.** Every run creates accounts (`load-<timestamp>-<n>`)
  and thousands of transfers. To reset the database:
  ```bash
  docker compose down -v
  docker compose up -d
  go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate \
    -path migrations \
    -database "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable" up
  ```
- **Resource use.** Each worker costs roughly 20 to 30 KB (goroutine, client
  connection, server connection). A few thousand workers are harmless. On
  Windows there are only about 16,000 ephemeral ports, so beyond that the tool
  starts seeing connection errors, which it counts as failures and stops. Keep
  `-max` at a sensible ceiling (for example `16384`) instead of huge values.
- **Abort at any time** with `Ctrl+C`. The API and PostgreSQL are not left
  corrupted: each transfer is one atomic transaction.

## Limitations of the measurements

- **The generator and the API share the same machine.** They compete for CPU,
  so absolute numbers are pessimistic. Use them to compare scenarios or
  strategies against each other, not as production capacity.
- **Closed-loop load.** Each worker waits for its response before sending the
  next request, so the offered load adapts to the API's speed. This shows the
  saturation point but hides coordinated-omission effects that an open-loop
  generator (fixed arrival rate) would expose.
- **Single run per configuration.** For a real comparison, repeat each
  configuration several times and record the environment (hardware, OS,
  PostgreSQL and Go versions, exact command), as the execution plan requires for
  the measurement phase.
- **Fixed 1-cent transfers and 1,000,000,000-cent funding.** Balances never run
  out, so `422 insufficient_funds` does not appear in these runs.
