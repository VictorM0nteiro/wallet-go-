# ADR 4 — Concurrency strategy chosen by measurement, not by preference

## Context

A transfer touches two accounts; two concurrent transfers touching an overlapping
account must not lose an update or drive a balance negative. Postgres offers more than
one correct way to guarantee that. Guessing which one is "obviously" better, without
measuring, is exactly the kind of engineering-by-taste this project's thesis (`docs/
escopo-carteira-go.md` §1) exists to argue against.

## Options considered

1. **Pessimistic row locking (`locking`).** `SELECT ... FOR UPDATE` on both accounts,
   always in ascending id order (`internal/adapters/postgres/transfer_executor.go`), so
   opposite-direction transfers queue behind the same first lock instead of
   deadlocking. Never aborts for concurrency; it waits.
2. **Optimistic concurrency via `SERIALIZABLE` (`serializable`).** No explicit locks;
   Postgres detects a conflicting read/write set at commit time and aborts one
   transaction (`40001`/`40P01`), which the executor retries with full-jitter backoff
   (`internal/adapters/postgres/serializable_executor.go`), up to 10 attempts, before
   giving up with `409 concurrent_conflict` — a status a client can safely retry, since
   nothing committed.

Both implement the same port (`app.TransferExecutor`), selected at startup with
`WALLET_TRANSFER_STRATEGY`, so the use case and the idempotency guarantee are identical
under either — only `Execute` differs. See `docs/concurrency-strategies.md` for the
full mechanism of each.

## Decision

`locking` is the default. The measured basis for this is
**[docs/performance-report.md](../performance-report.md)**: both scenarios tested
(`hot`, high contention on two accounts; `spread`, 100 accounts, little contention)
show `locking` ahead of `serializable`, by roughly 3× at the medians measured after
both strategies were optimized for round-trip count (`docs/bench/RESULTS.md` §9).

*The reasoning for* **why** *`locking` wins under this project's workload, and under
what contention profile the order would flip — is written in
`docs/performance-report.md`'s own "Why `locking` wins" and "Under what load would the
order flip?" sections, not duplicated here. This ADR records the decision and where its
evidence lives; the performance report is where the argument is made.*

## Consequences

- **The cost of `locking`:** every transfer on a hot account serializes behind the same
  lock. Session 9's `fsync` experiment (`docs/performance-report.md`) shows this
  plateau is dominated by commit durability, not the lock alone — a fact that would
  matter if this project ever had to defend why the `hot` scenario doesn't scale past
  ~400-500 req/s on this hardware.
- **The cost of `serializable`:** retried work is wasted work. Under the `hot`
  scenario it retries 10 000-25 000 times per run for a fraction of that in committed
  transfers, and a client must know to retry a `409`.
- **Switching strategies is one environment variable**, not a code change, which is the
  entire point of the port — this ADR could be revisited by re-running
  `docs/performance-report.md`'s method against a different workload shape without
  touching `internal/app/`.
