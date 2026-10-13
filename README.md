# wallet-go

A digital wallet API where the balance is never a stored column — it is derived from an
append-only ledger, and transfers between accounts are idempotent and correct under
concurrency.

> **Status:** through Session 14 of the [execution plan](docs/plano-execucao-wallet-go.md)
> — both concurrency strategies exist, measured and optimized; 5 ADRs, the performance
> report, and 4 of 6 induced-failure scenarios are written. See
> [Known limitations](#known-limitations) for who wrote the performance report's "why"
> section and which failure scenarios are missing.

## Architecture

Minimal hexagonal architecture: the domain has zero dependency on infrastructure, and the
transfer port is a single-method interface so the concurrency strategy underneath it can
be swapped without touching business rules — selected at startup by
`WALLET_TRANSFER_STRATEGY`, not by editing code.

```
                      ┌─────────────┐
   HTTP request  ──▶  │  httpapi    │  chi router, auth, DTOs,
                      │  (adapter)  │  domain-error -> HTTP status
                      └──────┬──────┘
                             │  app.TransferCommand / CashCommand
                             ▼
                      ┌─────────────┐
                      │   app       │  TransferService, CashService,
                      │ (use case)  │  idempotency — zero SQL here
                      └──────┬──────┘
                             │  app.TransferExecutor (port)
                             ▼
                 ┌───────────┴───────────┐
                 ▼                       ▼
         ┌───────────────┐      ┌────────────────────┐
         │ locking        │      │ serializable       │   two implementations
         │ SELECT FOR     │      │ SERIALIZABLE tx +   │   of the same port,
         │ UPDATE, both   │      │ retry on 40001/     │   swapped by env var
         │ accounts,      │      │ 40P01, full-jitter  │
         │ ascending id   │      │ backoff              │
         └───────┬────────┘      └─────────┬───────────┘
                 └───────────┬─────────────┘
                              ▼
                      ┌─────────────┐
                      │  Postgres   │  accounts, transfers, entries
                      │             │  (append-only), idempotency_keys
                      └─────────────┘
```

```
cmd/wallet/            binary entrypoint
internal/
  domain/              Money, AccountKind, CanDebit, typed errors
                        — zero import of pgx, net/http, or any infra package
  app/                 TransferService, CashService, AccountService, idempotency
  adapters/
    http/              chi router, DTOs, domain-error -> HTTP-status translation
    postgres/          repositories + the two transfer-port implementations
                        (locking, serializable)
cmd/loadtest/           standalone load generator, see docs/loadtest.md
```

Full rationale and the locked-in decisions live in
[docs/escopo-carteira-go.md](docs/escopo-carteira-go.md) and in [docs/adr/](docs/adr/).

## The ledger

A ledger is the immutable, ordered record of every movement of value in the system. There
is no `balance` column anywhere: an account's balance is always the sum of its entries.

Three properties define the model:

1. **Every movement has two sides.** A transfer of R$ 50 from Ana to Bruno is not one row,
   it is two, and they always sum to zero:

   | Account | amount_cents |
   |---|---|
   | Ana   | −5,000 |
   | Bruno | +5,000 |
   | **Sum** | **0** |

   This double-entry rule becomes a testable invariant: if a transfer ever fails to sum to
   zero, money was created or destroyed, and the test catches it before production does.
2. **Nothing is ever edited or deleted.** A wrong entry is reversed with another entry, never
   rewritten. The past stays true forever.
3. **Balance is derived, not stored.** `SELECT SUM(amount_cents) FROM entries WHERE
   account_id = ...` *is* the balance. Deposits and withdrawals are modeled as transfers
   to/from a single `system` account, which is the only account allowed to go negative.

## Running it

See **[docs/running-locally.md](docs/running-locally.md)** for the full step-by-step:
starting PostgreSQL, running migrations, running the API (`go run ./cmd/wallet` or
`docker compose up`), the complete `curl` walkthrough, and troubleshooting.

Short version, from an empty checkout:

```bash
cp .env.example .env
docker compose up -d
```

That brings up PostgreSQL and the API together (the API waits for the database to be
healthy). Migrations run as a separate one-shot service:

```bash
docker compose --profile tools run --rm migrate
```

## Endpoints

| Method | Path | Auth | Idempotency-Key |
|---|---|---|---|
| GET  | `/healthz` | no | — |
| GET  | `/readyz` | no | — |
| POST | `/accounts` | yes | no |
| POST | `/accounts/{id}/deposits` | yes | required |
| POST | `/accounts/{id}/withdrawals` | yes | required |
| POST | `/transfers` | yes | required |
| GET  | `/accounts/{id}/balance` | yes | — |
| GET  | `/accounts/{id}/entries` | yes | — |

Auth is a static API key in the `X-API-Key` header. An optional second key
(`WALLET_API_KEY_READONLY`) can read but not write: anything that moves money or creates
an account answers `403` for it. Full request/response shapes, error codes, and a
ready-to-import Postman collection are in [docs/running-locally.md](docs/running-locally.md).

`/healthz` reports only that the process is alive and never depends on the database;
`/readyz` reports whether the database is currently reachable. They are split on purpose —
a flaky database must not take down a container that is otherwise fine.

## Design decisions

Full reasoning in [docs/escopo-carteira-go.md](docs/escopo-carteira-go.md) §4 and in each
linked ADR:

- **`int64` cents, never float**, anywhere — not in the domain, not in logs, not in JSON.
  [ADR 2](docs/adr/0002-money-as-int64-cents.md)
- **Lock ordering.** A transfer locks both accounts in ascending `account_id` order,
  regardless of which is source and which is destination, so opposite-direction transfers
  never deadlock. [ADR 4](docs/adr/0004-concurrency-strategy-chosen-by-measurement.md)
- **Idempotency is scoped** by `(scope, endpoint, key)`, backed by the same database
  transaction as the transfer itself: claim the key, execute, store the response, or roll
  back all three together.
  [ADR 3](docs/adr/0003-idempotency-scoped-with-fingerprint.md)
- **Balance is a query, not a column.** The cost — a read becomes a `SUM` over N rows
  instead of a column read — is accepted and measured (`docs/performance-report.md`); a
  snapshot strategy remains an unpursued extra, see "Known limitations."
  [ADR 1](docs/adr/0001-balance-derived-from-ledger.md)
- **No external currency conversion.** Decided against, not an oversight — see
  [ADR 5](docs/adr/0005-no-external-currency-conversion.md) and "Known limitations."

## Load testing

`cmd/loadtest` ramps concurrent load against a running instance until it breaks, then
verifies the ledger stayed consistent. See [docs/loadtest.md](docs/loadtest.md).

The synthesized comparison between the two transfer strategies — the measurements that
matter, pulled out of the raw log — is
[docs/performance-report.md](docs/performance-report.md). The raw log itself, every run
and every command, is [docs/bench/RESULTS.md](docs/bench/RESULTS.md).

## Testing

```bash
go test ./...            # unit tests + integration tests (the latter need Docker)
go test -race ./...
```

Integration tests spin up their own disposable PostgreSQL via testcontainers-go.

## Known limitations

- **The performance report's "why" was drafted by Claude, not the author.** Both
  strategies exist, are measured, and are compared in
  [docs/performance-report.md](docs/performance-report.md) — but its sections on *why*
  `locking` wins and under what load that would flip were written by Claude Sonnet 5 at
  the author's request on 2026-10-13, after being deliberately left blank. The document
  says so. Treat that analysis as a draft to verify and restate, not as proof the author
  can defend it unprompted.
- **Induced-failure report covers 4 of 6 scenarios.** Postgres dying mid-load and coming
  back, `SIGTERM` under load, and `kill -9` mid-transfer are all run and documented in
  [docs/resilience-report.md](docs/resilience-report.md), with the hypothesis written
  before each one. Artificial latency injection (no tool set up for it) and an external
  dependency going down (conditional on the currency-conversion extra, not built — see
  [ADR 5](docs/adr/0005-no-external-currency-conversion.md)) are not attempted.
- **The extras queue was deliberately not pursued.** Two separate backlogs exist —
  [docs/escopo-carteira-go.md](docs/escopo-carteira-go.md) §11 (balance snapshot, currency
  conversion with resilience patterns, a third concurrency strategy, Terraform/Caddy
  deploy, outbox) and `docs/plano-execucao-wallet-go.md`'s X1-X7 (reversal, stable failure
  codes, three instances, reconciliation, outbox, an external reference key, Uber Fx) —
  mined mostly from a *different* job's take-home challenge to practice unrelated
  patterns, not required by this project's own thesis (§1 of the scope doc). X8 (least
  privilege: a restricted DB role, a hardened container) was the one exception worth
  doing, since it strengthens that thesis directly. The rest stays a backlog, not a gap:
  the project is considered complete with Sessions 1-14, and stopping scope on purpose is
  itself part of the answer to "why did you stop here."
- **Account creation is not idempotency-key gated.** It relies on the
  `UNIQUE(owner_id, kind, currency)` constraint instead (`409` on repeat), which is a
  narrower guarantee than the `Idempotency-Key` contract used by money-moving endpoints.
- No multi-currency, no real authentication beyond a static API key, no KYC, no scheduled
  or future-dated transfers, no payment system integration — all deliberately out of scope,
  see [docs/escopo-carteira-go.md](docs/escopo-carteira-go.md) §2.
- Balance reads degrade as the number of entries on an account grows, since there is no
  stored balance to read instead.
