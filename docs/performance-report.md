# Performance report — locking vs serializable (Session 10)

This is the synthesized report Session 10 of the [execution
plan](plano-execucao-wallet-go.md) asks for. The raw data — every run, every command,
every repetition — lives in [docs/bench/RESULTS.md](bench/RESULTS.md); this document
pulls the numbers that matter into one place and states the question plainly. The
**why** section is deliberately left for the author to write: that conclusion is the
point of this phase, not something handed over pre-written.

## The question

Which concurrency strategy sustains more transfers/second while keeping the
non-negative-balance invariant, under which contention profile — and what does each one
cost to get there?

## Method

| | |
|---|---|
| Scenarios | `hot` (every transfer touches the same 2 accounts) and `spread` (100 accounts, little contention) |
| Tool | `cmd/loadtest`, this project's own harness — see [loadtest.md](loadtest.md) |
| Repetitions | 5 per condition, alternating strategies in blocks |
| Seed | 42 (deterministic account/pair selection) |
| Environment | 6 cores / 12 threads, ~15 GiB, Windows 11 + Docker Desktop/WSL2, Postgres 16.15, Go 1.26 — full table in RESULTS.md |
| Pool | 30 connections (`WALLET_DB_MAX_CONNS`), decided in section 9 of RESULTS.md |

## Results

### Before optimization (RESULTS.md §6, stock Postgres config, pre round-trip reduction)

| Scenario | Strategy | Median req/s | Non-2xx | Retries/request |
|---|---|---|---|---|
| hot | locking | 350 | 0 | — |
| hot | serializable | 341 | 5.6% (`500`) | 1.05–1.73 |
| spread | locking | 2548 | 0 | — |
| spread | serializable | 741 | 2.3% (`500`), broke at 64 workers | 1.49–1.63 |

### After optimization (RESULTS.md §9: round trips 8→4, `409` replaces `500`, both strategies)

| Scenario | Strategy | Median req/s | Non-2xx | Retries/run |
|---|---|---|---|---|
| hot | locking | ~482 (342–497, fixed 32 workers) | 0 | — |
| hot | serializable | 397 | `409` only | 23 955–24 749 |
| spread | locking | **3551** (30 conns, 32 workers) | 0 | — |
| spread | serializable | **1160** | `409` only | 15 300–23 600 |

```
Throughput, after optimization, median req/s (log-ish scale, one tick ≈ 70 req/s)

spread / locking        [███████████████████████████████████████████████████] 3551
spread / serializable   [█████████████████]                                    1160
hot    / locking        [███████]                                              482
hot    / serializable   [██████]                                               397
```

### What moved and what didn't

- **Both strategies improved** from the same change (batching round trips); `locking`'s
  `spread` number moved the most in absolute terms (2548 → 3551), `serializable`'s moved
  the most in relative terms (741 → 1160, +57%).
- **`locking`'s gap over `serializable` did not close.** Before: ~3.4× on `spread` (2548
  vs 741). After: ~3.1× (3551 vs 1160). Narrower, not closed.
- **`serializable`'s retries per run fell on `spread` (66–70k → 15–24k) but rose slightly
  on `hot` (10–22k → 24–25k)** — opposite directions, both explained mechanically in
  RESULTS.md §9: shorter attempts leave a smaller window for conflict (helps low
  contention), but also let more attempts fit per second regardless of outcome (raises
  the count under maximum contention). Neither strategy's *correctness* changed: every
  run in both tables passed the ledger invariant check.
- **The `hot` plateau is mostly `fsync`, confirmed, not assumed.** Toggling
  `synchronous_commit` off (RESULTS.md §9) roughly doubled `hot`/`locking` throughput
  with zero code changes. That was never shipped as a config — it trades away the
  durability guarantee this project's thesis rests on — but it tells you where the
  `hot` number's ceiling actually comes from, and it isn't the row lock alone.

## Threats to validity

- **Single shared host.** Load generator, API, and Postgres all compete for the same 12
  threads. Session 9's own methodology sweep (RESULTS.md §9, "a methodology trap")
  caught this directly: raising workers to chase a higher pool ceiling added enough
  generator-side CPU noise to produce a non-monotonic, unreliable curve. Absolute
  numbers here are a relative comparison on one machine, not a production capacity
  claim.
- **Windows, not the Linux target of a real deployment.** `fsync` and socket I/O
  behavior differ under Docker Desktop/WSL2 from a native Linux Postgres host; the
  `getQueuedCompletionStatus`-dominated CPU profile in RESULTS.md §8 is itself partly a
  Windows-specific artifact of how blocking network I/O is implemented.
  `synchronous_commit`'s effect size might not transfer 1:1 to a different disk/OS.
- **Session-to-session noise exceeds some of the effects being measured.** The pool
  curve in RESULTS.md §2 swings up to ~30% between otherwise-identical runs; several
  individual numbers in the tables above (e.g. the single `hot`/`locking` run at 342 vs
  497, same settings) sit inside that same noise band.
- **Closed-loop load.** Every worker waits for its response before sending the next
  request (see [loadtest.md](loadtest.md)); this finds the saturation point but hides
  what an open-loop generator (fixed arrival rate) would show about queueing under a
  real traffic shape.

## Why `locking` wins here

*— for the author to write. The data above (throughput, retries, the 500→409 fix, the
`fsync` finding) is the evidence; the argument connecting it to the strategies'
respective mechanisms (row lock vs. optimistic snapshot + retry) is not written for
you here on purpose.*

## Under what load would the order flip?

*— for the author to write. `serializable` is closer to `locking` after the
optimization (3.1× instead of 3.4×) than before; is there a contention profile in
between `hot` and `spread` — or a lower retry ceiling, or fewer concurrent writers per
account — where it would cross over? The retry-count data above is the starting point
for that argument, not the argument itself.*

## The cost of each strategy

*— for the author to write: what `locking` gives up (transfers on one account queue
behind each other, full stop) against what `serializable` gives up (wasted retried
work, worse under contention, `409`s a correct client must know to retry).*
