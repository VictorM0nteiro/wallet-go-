# Concurrency strategies for transfers

A transfer moves money between two accounts, so two requests touching the same account
at the same time must not lose an update or drive the balance below zero. The database
is where that guarantee lives. This project implements two ways of getting it, selected
at startup with `WALLET_TRANSFER_STRATEGY`:

| Value | Mechanism | When it aborts |
|---|---|---|
| `locking` (default) | `SELECT ... FOR UPDATE` on both accounts, in ascending `account_id` order | Never aborts for concurrency. Waits. |
| `serializable` | Every transaction runs at `SERIALIZABLE`, with no explicit locks | Postgres aborts one of two conflicting transactions (`40001`, or `40P01` for deadlock). The executor retries. |

Both strategies implement the same port (`app.TransferExecutor`), so the use case does
not know which one is in use. Idempotency is the same in both: the claim of the
`Idempotency-Key`, the stored response and the transfer commit in one transaction.

## Locking

1. Lock both accounts with `SELECT kind FROM accounts WHERE id = $1 FOR UPDATE`, always
   in ascending order of id. Opposite-direction transfers (A to B and B to A) then queue
   behind the same first lock instead of deadlocking.
2. Read the source balance with `SUM(amount_cents)` over `entries`.
3. Ask the domain (`CanDebit`) whether the debit is allowed.
4. Insert the transfer and its two entries, then commit.

Cost: while a transaction holds the lock on a hot account, every other transfer on that
account waits in line. Throughput on one hot account is bounded by how long one
transaction takes.

## Serializable

1. Begin the transaction at `SERIALIZABLE`.
2. Read the kinds of both accounts and the source balance. No lock is taken.
3. Ask the domain (`CanDebit`) whether the debit is allowed.
4. Insert the transfer and its two entries, then commit.

If two transactions read and write overlapping data in a way that could not have run
one after the other, Postgres aborts one of them at some point, usually at commit. The
executor then:

- rolls the attempt back and runs the whole transaction again, from the beginning, so
  the balance is read again. A value read in a previous attempt is never reused, because
  that would bring back the lost-update bug that isolation exists to prevent;
- waits a random time up to an exponential ceiling (5 ms doubling, capped at 200 ms,
  full jitter) before the next attempt, so retrying transactions do not collide again in
  step;
- gives up after 10 attempts and returns `app.ErrConcurrencyConflict` (wrapping the last
  serialization failure), mapped to `409 concurrent_conflict` — nothing committed, so the
  same request, same `Idempotency-Key` included, is safe to retry;
- counts every retry. The count is exposed through `Retries()` and logged at shutdown.

Only `40001` and `40P01` are retried. Domain errors, such as insufficient funds, return
at once. The same applies to a deadline: the context is respected during the backoff.

Cost: under high contention, a transaction may do its work and then be thrown away. The
work is cheap, but it is wasted, and the wasted work grows with contention.

## Choosing

```bash
WALLET_TRANSFER_STRATEGY=locking        # default
WALLET_TRANSFER_STRATEGY=serializable
```

Any other value makes the process refuse to start.

## Testing

Both strategies run the same suites. The suites are in `transfer_executor_test.go` and
`idempotency_test.go`, and each test body receives a constructor for the strategy under
test. `strategies_test.go` owns the list of strategies, so a third strategy only needs an
entry there.

The suites cover: the happy path, insufficient funds leaving nothing behind, unknown
accounts, opposite-direction transfers, concurrent requests with the same idempotency key
producing exactly one transfer, replay of the original response, rejection of a reused key
with a different body, the in-flight conflict, and key scoping. Each test ends with the
two ledger invariants.

```bash
go test -race ./internal/adapters/postgres/...
```

These tests need Docker, since they start a PostgreSQL container with testcontainers.

## What the measurements decide

The strategies are compared with the same load tool used for the pool measurements (see
[loadtest.md](loadtest.md) and [bench/RESULTS.md](bench/RESULTS.md)). The question is not
which one is correct, since both pass the suites, but which one sustains more transfers
per second under each kind of contention, and what the cost of each is:

- **High contention** (the `hot` scenario, one pair of accounts): locking queues,
  serializable retries. Compare throughput, p99, and the retry count.
- **Low contention** (the `spread` scenario, 100 accounts): the expected overhead of
  serializable without conflicts, and whether locking pays for waiting that never happens.

Record the environment, the seed and the exact command with each run, as the rest of the
measurements do.
