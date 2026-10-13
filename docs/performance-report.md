# Performance report — locking vs serializable (Session 10)

This is the synthesized report Session 10 of the [execution
plan](plano-execucao-wallet-go.md) asks for. The raw data — every run, every command,
every repetition — lives in [docs/bench/RESULTS.md](bench/RESULTS.md); this document
pulls the numbers that matter into one place and states the question plainly.

The "why" sections below were drafted by Claude Sonnet 5 at the author's explicit
request, after initially being left blank on purpose — this project's own working
rules ask for that conclusion to come from the author, since defending it verbally
without the document in hand is the actual point. Read as a first draft to verify,
argue with, and restate in your own words, not as a finished answer.

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

The gap is not uniform, and the shape of the gap is the actual explanation, not just
the numbers themselves.

**On `spread` (low contention), `locking` is ~3× ahead (3551 vs 1160) — and that gap is
not made of waiting.** With 100 accounts and random pairs, two transfers rarely touch
the same account, so `locking`'s `FOR UPDATE` is almost always acquired instantly: an
uncontended row lock costs close to nothing. `serializable` has no equivalent "free
path." Postgres's `SERIALIZABLE` isolation tracks every row a transaction reads with a
predicate (SIREAD) lock and checks for a read/write dependency cycle at commit —
**on every transaction, whether or not it actually conflicts with anything.** That
bookkeeping is the cost `spread` is measuring: a tax `locking` simply does not pay when
there is nothing to wait for, and `serializable` pays unconditionally. Low contention
hides `locking`'s cost almost completely; it does not hide `serializable`'s.

**On `hot` (maximum contention), the two strategies converge (~482 vs ~397, roughly
1.2×, not 3×) — because this is the one profile where `locking`'s cost stops being
free.** Every transfer on the same two accounts queues behind the one already running,
so `locking`'s cost here is real queueing time, not bookkeeping. `serializable` adds
its retries on top of its already-present per-transaction tax, so both strategies are
now paying for contention, just in different currencies: `locking` pays in wait time
(nothing is wasted, every transaction that starts eventually commits or legitimately
fails on balance), `serializable` pays in wasted work (tens of thousands of retried
attempts per run, most of which are thrown away and redone).

**The mechanical summary:** `locking`'s cost is contention-proportional — close to zero
when accounts don't collide, real but bounded (queueing, not failure) when they do.
`serializable`'s cost has a contention-*independent* floor (the SSI bookkeeping on
every transaction) plus a contention-proportional retry cost on top. `locking` wins on
`spread` because that floor dominates when there's nothing to retry. It wins on `hot`
too, but by much less, because at that point both strategies are paying for the same
real contention — `locking` just pays for it without throwing work away.

## Under what load would the order flip?

Not under either scenario actually measured here — `locking` wins both, and by a wide
margin on the one (`spread`) that was supposed to be `serializable`'s best case.

The honest answer is that this project's two scenarios don't exercise the shape of
workload where `SERIALIZABLE` is supposed to win in general: many concurrent **readers**
mixed with writers (where `locking`'s explicit locks would block reads that
`SERIALIZABLE` could let through, since SSI only aborts on an actual dependency, not on
mere overlap), or transactions touching **more than two rows**, where acquiring several
ordered locks starts to cost more than tracking a few predicates. Every transfer in
this project locks exactly two rows (`lockOrder`), which is cheap to acquire even under
real contention — it never gets to the point where `locking`'s approach has to do
something expensive that `serializable` avoids. A workload shaped differently from
"move money between exactly two accounts" might flip this; nothing measured in this
report tests that shape, so this is a hypothesis, not a finding, and the honest
conclusion for *this* application is: don't expect the order to flip under more load of
the same kind, only under a different kind of load this project doesn't generate.

## The cost of each strategy

**`locking`'s cost is latency that grows but requests don't fail.** A transfer on a hot
account waits behind whatever is already queued on that account; the request gets
slower under load, but a request that starts always finishes — it either commits or
fails for a real reason (insufficient funds, unknown account), never because of
contention itself. There is no `maxAttempts`, no ceiling where a legitimate request
starts getting rejected just for showing up at a busy moment.

**`serializable`'s cost is work thrown away, and sometimes an outright failure the
client has to handle.** Tens of thousands of retried attempts per run on both
scenarios measured — real database work (reads, inserts, a rollback) that produces
nothing. And unlike `locking`, a request *can* fail purely from contention: after 10
attempts, `409 concurrent_conflict` with no transfer applied, even though nothing was
wrong with the request itself. A client has to know this is retryable and do something
about it; `locking` never puts that burden on the caller.

**For a ledger, that asymmetry matters as much as the throughput numbers.** This
project's own thesis is about correctness holding under real concurrency, not just
speed. `locking` degrades the way a financial system should prefer to degrade — slower,
never wrong, never an avoidable error for a legitimate request. `serializable` is
"correct" too (every passed test and every measured run proves it), but it pushes some
of the cost of contention onto the caller instead of absorbing it, which is a worse
operational property for this kind of system even where its raw throughput were closer
to `locking`'s than it is here.
