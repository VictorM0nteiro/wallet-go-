# ADR 9 — Elastic Postgres connection pool for cmd/wallet

## Context

The Grafana dashboard provisioned in ADR 8 captured `wallet_db_pool_acquired_conns`
saturating hard at `WALLET_DB_MAX_CONNS` (fixed at 30) during a `cmd/loadtest` burst
(`docs/img/grafana-dashboard.png`). The pool size had been a single static number
since the project started, picked once and never revisited under real measurement.

`pgxpool.Pool` (jackc/pgx v5.11.0) has **no public API to change `MaxConns` after
`NewWithConfig`** — confirmed by reading the pgx source directly, not assumed. Any
real elasticity has to live above pgxpool, not inside it.

## Options considered

1. **Recalibrate `MinConns`/`MaxConns`/`MaxConnIdleTime` as static config.**
   Rejected — not because it's wrong, but because it answers a different question
   than the one asked here. pgxpool already does this much on its own (lazy
   connection creation up to `MaxConns`, idle shrink down to `MinConns`); picking
   better fixed numbers would help, but it is still one number chosen in advance,
   not something that reacts to the load actually happening.
2. **An external pooler (pgbouncer or similar) in front of Postgres.** Not explored
   in depth, but worth naming: this is the "obvious other way" to get elasticity,
   and it has real advantages (connection multiplexing across processes, not just
   within one). Rejected here because it adds an operational component this
   project's single-process-plus-relay shape doesn't otherwise need, and because
   the interesting part of this pass — proving an in-process elastic mechanism is
   even buildable against pgxpool's constraints — is exactly what an external
   pooler would make moot.
3. **A swappable `*pgxpool.Pool` behind `postgres.Pool`, resized by a background
   policy loop (chosen).** The only one of the three that reacts to real-time load
   signal rather than a number chosen in advance.

## Decision

**`postgres.Pool` holds its underlying `*pgxpool.Pool` behind a `sync.RWMutex`, not
a bare pointer.** Every forwarding method (`Exec`, `Query`, `QueryRow`, `Begin`,
`BeginTx`, `SendBatch`, `Stat`, `Ping`) holds `mu.RLock()` for its *entire*
duration, not just long enough to read the pointer. A resize can only take
`mu.Lock()` — exclusive — once every in-flight call has finished, which is what
makes it safe to close the old `*pgxpool.Pool` immediately afterward.

This wasn't the first design. An `atomic.Pointer[pgxpool.Pool]` version — cheaper to
read — shipped first and passed its own unit tests. A real integration test
(`pool_resize_test.go`, sustained concurrent load driven across an actual resize
against a live container) then failed with `closed pool` errors: a goroutine could
load the old pool's pointer an instant before a swap, and only call `Exec` on it an
instant after the background goroutine had already closed it. `atomic.Pointer` made
the *swap* atomic, not "swap plus every call already in flight against the old
pointer" — those are different guarantees, and only the second one is actually
needed. `sync.RWMutex` closes that gap by construction: by the time a resize holds
the exclusive lock, nothing can still be mid-call against the pool it's about to
replace.

**An elastic pool starts at `MinConns` (the floor), not `MaxConns` (the ceiling),
and grows under sustained load.** The first version of `NewPool` always created the
initial `pgxpool.Pool` at the configured ceiling — which meant `Grow` could never
have anywhere to go. This is also how the project's own test caught the issue:
`TestElasticPool_GrowsUnderSustainedLoadWithoutBreakingInFlightQueries` starts a
pool at a small floor and asserts `Stat().MaxConns()` actually increases under load.

**The decision of *when* to resize is a pure function, separate from the pgx side
effects.** `resizePolicy.decide` (`pool_resize_policy.go`) takes a saturation ratio
and a small piece of state (consecutive-tick streaks, time of last resize) and
returns Grow/Shrink/Hold — no pgx, no goroutine, no I/O. `pool_resize.go` is the
thin layer that samples `Stat()`, calls the pure function, and — only on
Grow/Shrink — builds a new `pgxpool.Pool`, `Ping`s it (aborting the resize if the
ping fails, e.g. the Postgres server is itself at its own `max_connections`), and
performs the locked swap above. This split is what makes the policy table-testable
in milliseconds (`pool_resize_policy_test.go`, no Docker) separately from the
integration test that exercises the real swap.

**Only `cmd/wallet` gets this; `cmd/relay` does not.** The relay's own pool
(`MaxConns: 5`, hardcoded) has never been observed saturating in any dashboard
capture and is small and fixed by design — `MinConns` is left at zero there, which
is also the mechanism's off switch (`PoolConfig.MinConns == 0` means "behave exactly
as before, no extra goroutine").

**The watermarks, hysteresis, cooldown, and step size are constants
(`pool_resize.go`), not environment variables — only the floor (`WALLET_DB_MIN_CONNS`,
new) and the existing ceiling (`WALLET_DB_MAX_CONNS`) are operator-tunable.**

<!-- author: state here, in your own words, why you picked these specific numbers
(high watermark 85%, low watermark 30%, 3-tick hysteresis, 3s check interval, 45s
cooldown, step of 8 connections) — what tradeoff each one is making (reaction speed
vs. noise sensitivity for the watermarks/hysteresis/interval; avoiding resize
pile-up vs. reacting to a second burst for the cooldown; simplicity vs. overshoot
for a fixed step instead of a percentage), and whether the one real burst this
project measured (the ADR 8 dashboard capture) actually supports these specific
values or whether they're still a guess waiting on more data. -->

## Consequences

- **A new goroutine per elastic pool, with its own shutdown ordering.**
  `Pool.Close()` now also stops the resize loop (`poolResizer.stop()`, a
  `sync.WaitGroup` join) before closing the current `pgxpool.Pool`, so no resize can
  be mid-flight when the process is shutting down. This widens `Close()`'s contract
  slightly — it now joins a goroutine, not just closes connections — but callers
  don't need to know or do anything differently.
- **New metric:** `wallet_db_pool_resizes_total{direction="grow"|"shrink"}`
  (`metrics.go`), incremented on every successful swap. `wallet_db_pool_max_conns`
  (ADR 8) needed no changes — it already reads `Stat()` fresh every scrape, so it
  reflects the live size automatically, before and after any resize.
- **A resize briefly blocks new queries, by design.** While a resize holds
  `mu.Lock()` to swap the pool pointer, every `Exec`/`Query`/etc. call blocks until
  it's released — bounded by however long building and pinging the new pool takes
  (typically well under a second against a healthy Postgres). This is a deliberate
  trade: a short, bounded stall under an exclusive lock, instead of the alternative
  this project tried first and rejected — an unbounded number of `closed pool`
  errors reaching real callers.
- **No test exercises the `Ping`-failure-during-resize path against a real broken
  server.** The code path exists (`pool_resize.go`'s `resize` aborts and logs if the
  new pool's `Ping` fails) but manufacturing that failure cleanly against a real
  Postgres container — without adding a test-only seam to production code purely
  for this — proved impractical in the time this pass had. Accepted as a documented
  gap, the same kind of call as `docs/resilience-report.md`'s unattempted scenarios.
- **No hard cap on how many old pools can be draining concurrently.** If resizes
  somehow outpaced how fast an old pool's in-flight connections close, old pools
  could in principle pile up. The cooldown is sized to make this unlikely in
  practice, but nothing enforces it structurally — unmeasured, same posture as the
  outbox's unbounded growth (ADR 7).
