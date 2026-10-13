# Resilience report — Phase E

Each scenario's hypothesis is written before running it. Wrong hypotheses stay in the
text — the divergence between predicted and observed is the actual product of this
phase, not a mistake to edit away.

## Roteiro 1 — Postgres dies mid-load

**Setup:** `locking` strategy, light load against `/transfers` and `/accounts/{id}/deposits`
while `docker compose stop postgres` is run mid-run.

**Hypothesis, written before running:**

1. `/healthz` keeps returning `200 {"status":"ok"}` throughout — it has no DB dependency
   (`internal/adapters/http/health.go`).
2. `/readyz` starts returning `503 {"status":"unavailable"}` within ~2s of Postgres going
   down (`readyTimeout` in `health.go`), since `Ping` will fail or time out.
3. In-flight writes that haven't yet acquired a pool connection fail with
   `503 service_unavailable` once `AcquireTimeout` (3s, `cmd/wallet/main.go`) expires —
   `statusFor` maps `context.DeadlineExceeded` to `503`.
4. **Open question, no prediction:** what status a write gets when its connection breaks
   *mid-query* (not a timeout, a network error) is not obviously covered by
   `internal/adapters/http/errors.go`'s `statusFor` — it only has a case for
   `context.DeadlineExceeded`, nothing for a raw `pgconn`/network error. The default
   branch is `500 internal_error`. Whether that's what actually happens is what this
   roteiro checks.
5. The app process itself does not crash and keeps serving `/healthz`.
6. No transfer is left half-applied — every response that claims success has a matching
   committed transaction; the ledger invariants still hold after.

**Observed:** 35 ticks, 1/s, against `locking`, a 1-cent transfer plus `/healthz` and
`/readyz` each tick. `docker compose stop postgres` at tick 8, `docker compose start
postgres` at tick 22.

| Ticks | `/healthz` | `/readyz` | transfer |
|---|---|---|---|
| 1–7 (before) | 200 | 200 | 201 |
| 8–21 (Postgres down) | **200, every tick** | 503 | **503 `service_unavailable`, every tick** |
| 22–35 (after) | 200 | 200 | 201 |

- **Prediction 1 confirmed exactly.** `/healthz` never left `200`, all 35 ticks.
- **Prediction 2 confirmed, faster than expected.** `/readyz` flipped to `503` on the
  very first tick after the stop (tick 8), not after the 2s `readyTimeout` — `Ping`
  against a closed port fails immediately (connection refused), it doesn't need to time
  out.
- **Prediction 3 confirmed.** Every write during the outage got `503
  service_unavailable`, not a hang, not a crash.
- **Open question (4) resolved, and resolved the easy way.** Every failure during the
  outage was `503`, never `500`. The code path that would have produced an unhandled
  `500` (a raw network error with no `statusFor` case) never gets reached here: once
  Postgres is unreachable, `pgxpool.Acquire` keeps trying to get a connection until the
  pool's own `AcquireTimeout` (3s) expires, and that always surfaces as
  `context.DeadlineExceeded` — which `statusFor` does map to `503`. The gap in
  `errors.go` (no case for a raw connection error) is still real in principle, but this
  scenario doesn't exercise it, because the acquire timeout gets there first.
- **Prediction 5 confirmed.** The app process never restarted; same PID serving
  `/healthz` the whole time (checked via `docker compose ps`, not shown above).
- **Prediction 6 confirmed, checked after recovery:** 7 transfers before the outage + 14
  after = 21 successful 1-cent transfers. Balances: `999979` / `21` — exactly 21 cents
  moved, nothing lost, nothing duplicated, nothing partially applied.

## Roteiro 2 — Postgres comes back

Covered by the same run as roteiro 1 (ticks 22–35), since recovery is the natural
continuation of the outage, not a separate process to restart.

**Hypothesis, written before running:** `pgxpool` reconnects on its own once Postgres
accepts connections again — no code exists anywhere to detect "the database is back"
and do something about it, so if recovery doesn't "just work," that absence is the bug.
`/readyz` should return to `200` as soon as a `Ping` succeeds, and the very next write
after that should succeed too, without restarting the app.

**Observed:** confirmed, and with no visible transition period — tick 22, the first
tick after `docker compose start postgres`, already shows `/readyz=200` and a
successful `201` transfer. Recovery was not gradual; it was binary between tick 21 and
tick 22. No manual intervention, no app restart, nothing in the logs about reconnecting
— `pgxpool`'s own connection management handled it silently, which is exactly what
"no code needed" predicted.

## Roteiro 3 — `SIGTERM` under load

**Hypothesis, written before running:**

1. `docker compose stop app` sends `SIGTERM` to PID 1 directly — the `Dockerfile`'s
   `ENTRYPOINT` is exec form (`["/wallet"]`, no shell), so there's no shell in between
   to swallow the signal. `signal.NotifyContext` catches it.
2. New connections during the drain get refused (visible to `cmd/loadtest` as
   `conn_fail`), not a `5xx` — the listener stops accepting once `srv.Shutdown` runs.
3. In-flight requests finish and return their real response within the shutdown window:
   each request's own `RequestTimeout` (5s) is comfortably under the shutdown deadline
   (10s, `cmd/wallet/main.go`).
4. The process exits with code `0`, inside Docker's stop grace period, no `SIGKILL`
   needed.
5. **A risk flagged before running, not found by inspection after:** the code's own
   shutdown deadline (10s) and Docker's default `stop_grace_period` (10s, not
   overridden in `docker-compose.yml`) are the *same* value. Both clocks start at the
   same `SIGTERM`. If graceful shutdown ever took close to its own 10s limit, Docker's
   `SIGKILL` could land first, with no margin between them. Prediction: invisible under
   this test's light load, but latent regardless of what this run shows.
6. No partial transfers; whatever committed before the stop stays correct.

**Observed:** `cmd/loadtest -scenario spread -start 8 -max 8 -stage 20s`, `docker compose
stop app` issued 5s into the stage.

- **Predictions 1 and 4 confirmed, and faster than expected.** `SIGTERM` at
  `22:45:08.268`; container `FinishedAt` `22:45:08.594` — **~326ms** from signal to
  exit, `ExitCode=0`. Nowhere near the 10s budget; the risk flagged in prediction 5
  (the two 10s clocks racing) is real but this load was too light to approach it. Not
  resolved either way by this run.
- **Prediction 3 confirmed, visible in the app's own log.** The last dozen-plus
  `201 /transfers` lines are timestamped *after* `"wallet: shutting down"` was logged —
  in-flight requests kept completing and returning their real response during the
  drain, not getting cut off.
- **Prediction 2 confirmed, violently.** `cmd/loadtest`'s own summary
  (`docs/bench/spread-seed7-20261013-194504.json`): `conn_fail 97.0%`,
  `connection_refused: 244332`. New connections were refused immediately once
  the listener closed — correct behavior — but the *count* is a closed-loop load
  generator spinning as fast as the OS can refuse a connection for the ~15s the app
  stayed down, not 244k real client requests. Expected given the setup, not a finding
  about the app.
- **Prediction 6 confirmed**, checked directly against Postgres rather than trusting
  `cmd/loadtest`'s own consistency check (see below for why): zero transfers with a
  non-zero entry sum, zero customer accounts with a negative balance, across the whole
  database at the time.

**A real divergence — in the test, not the app.** `cmd/loadtest` reported "Did NOT
recover within 60s" and exited `1`. That is wrong, and the cause is this test
script's own sequencing: it called `docker compose stop app`, then immediately `wait`ed
on the `loadtest` process — but `loadtest` enters its own up-to-60s recovery-polling
loop right after the break, and the app was not brought back up (`docker compose up -d
app`) until *after* that `wait` returned. The app was kept down for the entire window
`loadtest` was polling it, which guarantees "did not recover" regardless of how the app
actually behaves. This is the "roteiro 3 quase sempre quebra" the plan warns about —
it broke, just not where the hypothesis expected. The fix is bringing the app back up
concurrently with (or before) waiting on the load generator, not after. Not re-run with
the fix here, since the manual ledger check above already answers the only thing that
mattered (correctness survived the stop); the direct `psql` check replaces the
automated one for this scenario because of this script bug.

## Roteiro 5 — `kill -9` mid-transfer

Session 14 in the plan, done here alongside Session 13 since the setup (load generator
+ Docker signal) is identical; roteiro 4 (artificial latency) is deferred, see "Still
open" below.

**Hypothesis, written before running:** this one isn't really testing the app's code —
`SIGKILL` gives it no chance to run any of it, no `defer`, no shutdown log, nothing.
It's testing whether **Postgres's own transaction semantics**, not this project's Go
code, are what actually protect correctness. A transaction that hasn't committed when
its client connection dies gets rolled back by Postgres itself, unconditionally — that
guarantee has nothing to do with how the client died. So:

1. `docker compose kill app` (`SIGKILL` by default) exits with no graceful-shutdown log
   line at all — unlike roteiro 3, there is no `"wallet: shutting down"` here.
2. Every transfer either committed in full before the kill, or never committed at all —
   never half-applied. This should hold even for whatever transaction was literally
   open on the wire at the instant of the kill, because Postgres rolls back on a
   dropped connection regardless of why it dropped.
3. Ledger invariants hold after restart, same as every other roteiro.
4. A client that sent a request and got no response (because the server died before
   answering) can safely retry with the same `Idempotency-Key` once the app is back —
   if the original attempt had committed, it replays; if not, a fresh attempt runs.

**Observed:** seeded two accounts, fired 15 concurrent `POST /transfers` (keys
`kill9-r1`..`kill9-r15`, 1 cent each) with no delay, then `docker compose kill app`
(`SIGKILL`) immediately after.

- **Prediction 1 confirmed.** `ExitCode=137` (128+9, the kernel's own signature for
  "killed by signal 9"), and no `"wallet: shutting down"` line anywhere in the app's
  log — unlike roteiro 3, there was no graceful path at all to log it.
- **Prediction 2 and 3 confirmed, with precise evidence from `idempotency_keys`, not
  just the final balance:**

  | Key | State after the kill |
  |---|---|
  | `kill9-r2` .. `kill9-r11` (10 keys) | `completed` — committed before the kill (visible in the app's own request log, each logged `201` with a 2-8ms duration, all before the kill timestamp) |
  | `kill9-r1` | **no row at all** after the kill — the whole transaction (claim, execute, store response) rolled back together, exactly as the idempotency design claims it will |
  | `kill9-r12` .. `kill9-r15` (4 keys) | **no row at all** — killed before even the claim committed |

  Retrying `kill9-r1` with the same key after recovery did not replay a stored
  response — it ran a **fresh** transfer and got a **new** `transfer_id`
  (`f44dae95-...`, different from any of the 10 that committed originally). That is
  only possible if `kill9-r1`'s original attempt left no trace at all, confirming
  prediction 2 precisely: not "probably rolled back," but directly observable as
  rolled back, because a *committed* key would have replayed instead of executing
  again.
- **Prediction 3 confirmed for all 15, not just the retried one.** Every key is either
  fully `completed` (11, after the retry) or entirely absent (4) — none stuck
  `in_flight`, none with a transfer row but a mismatched entry sum. There is no
  intermediate state anywhere in the data.
- **Prediction 4 confirmed.** After the retry, final balances: `999989` / `11` — 10
  original commits + 1 successful retry = 11 cents moved, exactly matching 11
  `completed` keys. Direct invariant check against Postgres: zero transfers with a
  non-zero entry sum, zero customer accounts negative.
- **The 4 never-retried keys (`kill9-r12`-`r15`) are not a bug, they're an honest gap in
  this test, not the app.** A real client holding one of those `Idempotency-Key`s would
  need to decide to retry; this test demonstrated the pattern once (`kill9-r1`) as
  proof, not for all 15. Nothing here suggests the other 3 (besides `r1`) would behave
  differently if retried.

## Summary

| Roteiro | Result |
|---|---|
| 1 — Postgres dies | All 6 predictions confirmed; the one open question resolved cleanly in the app's favor |
| 2 — Postgres comes back | Confirmed, instant, no code needed |
| 3 — `SIGTERM` under load | App behaved correctly; the test's own recovery check was wrong because of a sequencing bug in the test script, documented above as the real divergence found |
| 5 — `kill -9` mid-transfer | All predictions confirmed, with key-level evidence, not just a balance check |

Three for three on the predictions that were actually about the app. The one genuine
"it broke" the plan warned about (roteiro 3) broke in the test harness, not the
application — which is itself worth saying out loud rather than quietly fixing and
re-running: a resilience test that produces a false negative because of its own
orchestration is a real failure mode of *testing* resilience, distinct from the
application's resilience itself.

## Still open

- **Roteiro 4 (artificial latency injection)** — not attempted. Needs a tool this
  project doesn't have set up (`tc netem` isn't available the same way on Docker
  Desktop/Windows as on native Linux; a TCP proxy like `toxiproxy` would need to be
  added as a new dependency). Deferred, not done, same spirit as the extras queue.
- **Roteiro 6 (external dependency down)** — does not apply. It was conditional on the
  extra 2 (external currency conversion) existing; [ADR 5](adr/0005-no-external-currency-conversion.md)
  records the decision not to build that.
- **The latent gap found in roteiro 1's hypothesis 4** — `statusFor` has no case for a
  raw connection/network error (as opposed to a timeout), and would fall through to
  `500 internal_error` if one ever reached it directly. Not exercised by any scenario
  run here, because `AcquireTimeout` always gets there first in practice. Worth a
  dedicated unit test if this report's absence of evidence is ever treated as evidence
  of absence.
- **`resilience3.sh`'s sequencing bug** is fixed in reasoning here, not in a committed
  script — the scripts used for this phase live in the session's scratch space, not in
  the repository, matching how the earlier bench scripts were handled.
