# ADR 1 — Balance derived from an append-only ledger, never a stored column

## Context

The obvious model is a `balance` column updated with `UPDATE accounts SET balance =
balance - 50`. It is simpler, and it is what most portfolio projects do. The problems
show up in production, not in `docker compose`:

- **No history.** If the balance is wrong, there is no way to find out when or why. You
  have a number, not an explanation.
- **Divergence is silent.** A bug that drops an `UPDATE` leaves the balance wrong forever
  and nobody is notified. With a ledger, the same bug breaks the zero-sum invariant and
  shows up in a test or a reconciliation.
- **Auditability is impossible.** No system handling other people's money survives
  without being able to answer "where did this value come from."

## Options considered

1. **Stored `balance` column**, updated in place on every transfer.
2. **Append-only ledger** (`entries` table): every movement is a row, balance is
   `SELECT SUM(amount_cents) FROM entries WHERE account_id = ...`, and that query *is*
   the balance — there is no column to drift from it.

## Decision

Option 2. There is no `balance` column anywhere in the schema. Every deposit,
withdrawal and transfer is one or two rows in `entries`, and a transfer's two entries
always sum to zero (double-entry). This is enforced and tested as an invariant — see
`internal/adapters/postgres/transfer_executor_test.go` and the CI-run invariant checks
in `docs/escopo-carteira-go.md` §3.

## Consequences

- **The cost is real and accepted.** Reading a balance is an aggregation over N rows
  instead of a column read. Session 9's profiling
  (`docs/bench/pprof/`, `docs/performance-report.md`) shows this cost is small relative
  to the network round trip itself, but it does grow with the number of entries on an
  account — documented as a known limitation in the README.
- **Mitigation, not built:** a balance snapshot (consolidated balance up to a cutoff
  `entry_id`, current balance = snapshot + sum of entries after it) is in
  `docs/escopo-carteira-go.md` §11's extras queue, not pursued — see the README's
  "Known limitations" for why.
- **The win is structural, not just a feature:** the two ledger invariants (zero-sum per
  transfer, non-negative balance per customer account) are things a test can check
  mechanically. A stored-balance design has no equivalent invariant to check against —
  the column just is whatever the last `UPDATE` left it.
