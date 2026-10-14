# ADR 7 — Transactional outbox + polling relay, no broker

## Context

X5 (`docs/plano-execucao-wallet-go.md`) asks for a "transferência concluída" event
recorded in the same SQL transaction as the transfer it describes, delivered by a
separate process that polls, with a test that interrupts delivery and confirms the
event is redelivered with the same `event_id`, never lost. This is the same shape as
ADR 3's idempotency guarantee: the claim, the transfer and the stored response commit
or roll back together — here, an event joins that same transaction.

An external proposal, pasted during this session, suggested going further: deliver
through a Redis Stream and build a consumer that maintains a materialized balance
(CQRS). That contradicts a sentence already written into this project's own extras
backlog introduction — "itens que cabem no modelo do wallet sem trocar a tese central
(saldo derivado, **sem broker**, API key estática)" — confirmed at the dependency
level: `go.sum` has no redis/rabbitmq/kafka/nats/amqp-shaped package before or after
this change. Only the outbox+relay part (X5 as originally scoped) was built.

## Options considered

1. **Publish directly from the HTTP handler or use case**, right after `Execute`
   succeeds. Rejected: the process can die between the transaction commit and the
   publish call, losing the event with no way to know it was lost — the exact failure
   mode an outbox exists to prevent.
2. **A message broker** (Redis Stream, RabbitMQ, Kafka). Rejected on this project's
   own terms, not evaluated on technical merit: it would mean adding a broker
   dependency, which the backlog's own introduction already rules out, and would
   require rewriting ADR 1's "saldo derivado, sem broker" framing and this project's
   closed-project narrative (README, `docs/plano-execucao-wallet-go.md`) to justify.
3. **Transactional outbox + polling relay (chosen).** The event write shares the
   transfer's own transaction — `AppendEvent` is a 5th method on `app.TransferTx`,
   called from `TransferService.Transfer` right after `Execute` succeeds, inside the
   same `InTx` closure `ClaimKey`/`CompleteKey` already use for the idempotency
   guarantee. A separate process (`cmd/relay`) polls for unpublished rows and delivers
   them at least once.

## Decision

**At-least-once, not exactly-once; `event_id` is generated once and never
regenerated.** `PollOnce` processes one row per database transaction: `SELECT ...
FOR UPDATE SKIP LOCKED LIMIT 1` → `Publisher.Publish` → mark published (or record the
failed attempt and reschedule) → commit. If the process dies after `Publish` returns
but before that commit, nothing for that row is persisted — not even the fact that it
was attempted — so the next poll claims it again and redelivers it with the same
`event_id`. <!-- author: state here, in your own words, why one transaction per row
was chosen over one transaction per whole batch, and what breaks with the batch
version (next_attempt_at/attempts bookkeeping for a row that keeps failing gets
rolled back along with the row that failed next to it) -->

**`SELECT ... FOR UPDATE SKIP LOCKED`**, so more than one relay instance could run
without two of them publishing the same row at the same time — even though exactly
one instance runs today. The guarantee is about what the code permits, not how many
copies happen to be deployed, the same reasoning behind locking both accounts in
ascending `account_id` order regardless of which direction a transfer runs.

**A new round trip per transfer.** `Execute`'s own `pgx.Batch` calls are already sent
and closed by the time `Transfer` calls `AppendEvent` — there is no batch left to
queue onto. <!-- author: say whether you measured this, or are accepting it
unmeasured and why that's fine for this feature specifically -->

**`wallet_relay`, a role separate from `wallet_app`.** Extends the X8 least-privilege
pattern instead of reusing `wallet_app` for convenience: `wallet_app` only ever
`INSERT`s into `outbox` (it never reads or marks it); `wallet_relay` only ever
`SELECT`s and marks `published_at`/`attempts`/`next_attempt_at` — it has no grant at
all on `accounts`, `transfers`, `entries`, or `idempotency_keys`. A bug or a
compromise in the relay process cannot reach the ledger, regardless of instance
count. `TestWalletRelayLeastPrivilege` proves this against a real connection, the
same way `TestWalletAppLeastPrivilege` already does for `wallet_app`.

**No broker, honestly.** `Publisher` is a Go interface
(`internal/adapters/postgres/outbox.go`) with one shipped implementation,
`LogPublisher`, which only logs the event. A real deployment would swap it for a
webhook call or a notification service — this project does not build one, on
purpose, the same way it declined the rest of its extras backlogs.

## Consequences

- **Published rows are never deleted.** The partial index
  (`WHERE published_at IS NULL`) keeps the one query the relay runs cheap no matter
  how many published rows accumulate — what grows unbounded is disk, not query
  latency. Retention/cleanup is an accepted, unmeasured cost for this pass, the same
  kind of call ADR 1 already made about `SUM(entries)` as the balance.
- **`LogPublisher` delivers nothing real.** It exists to prove the mechanism (outbox
  write, poll, at-least-once redelivery, `SKIP LOCKED`), not to be a production
  integration. Anything that actually consumes these events still needs to be
  written, and would need to be idempotent by `event_id` itself — this project
  guarantees delivery, not exactly-once processing on the other end.
- **A relay failure on one row in a batch does not affect the others.** Each row
  commits independently; `TestOutboxRelay_OneFailureInABatchDoesNotAffectOtherRows`
  proves a failed delivery neither loses nor duplicates its neighbors.
- Deposits and withdrawals get this for free, with no separate wiring: both are
  modeled as transfers against the system account, and `AppendEvent` is called from
  inside `TransferService.Transfer` itself.
