# wallet-go

A digital wallet API where the balance is never a stored column — it is derived from an
append-only ledger, and transfers between accounts are idempotent and correct under
concurrency.

> **Status:** through Session 6 of the [execution plan](docs/plano-execucao-wallet-go.md)
> (the "cut point" — apresentável mesmo que pare aqui). Measurement results, ADRs, and the
> induced-failure report belong to later phases and are not written yet; see
> [Known limitations](#known-limitations).

## Architecture

Minimal hexagonal architecture: the domain has zero dependency on infrastructure, and the
transfer port is a single-method interface so the concurrency strategy underneath it can
be swapped without touching business rules.

```
cmd/wallet/            binary entrypoint
internal/
  domain/              Money, AccountKind, CanDebit, typed errors
                        — zero import of pgx, net/http, or any infra package
  app/                 TransferService, CashService, AccountService, idempotency
  adapters/
    http/              chi router, DTOs, domain-error -> HTTP-status translation
    postgres/          repositories + the transfer-port implementation (row locking)
cmd/loadtest/           standalone load generator, see docs/loadtest.md
```

Full rationale and the locked-in decisions live in
[docs/escopo-carteira-go.md](docs/escopo-carteira-go.md).

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

Auth is a static API key in the `X-API-Key` header. Full request/response shapes, error
codes, and a ready-to-import Postman collection are in
[docs/running-locally.md](docs/running-locally.md).

`/healthz` reports only that the process is alive and never depends on the database;
`/readyz` reports whether the database is currently reachable. They are split on purpose —
a flaky database must not take down a container that is otherwise fine.

## Design decisions

These are locked in (full reasoning in [docs/escopo-carteira-go.md](docs/escopo-carteira-go.md)
§4); ADR write-ups land in Session 11 and will be linked from here:

- **`int64` cents, never float**, anywhere — not in the domain, not in logs, not in JSON.
- **Lock ordering.** A transfer locks both accounts in ascending `account_id` order,
  regardless of which is source and which is destination, so opposite-direction transfers
  never deadlock.
- **Idempotency is scoped** by `(scope, endpoint, key)`, backed by the same database
  transaction as the transfer itself: claim the key, execute, store the response, or roll
  back all three together.
- **Balance is a query, not a column.** The cost — a read becomes a `SUM` over N rows
  instead of a column read — is accepted and will be measured (Session 7-9); a snapshot
  strategy is the first item in the extras queue if it turns out to matter.

## Load testing

`cmd/loadtest` ramps concurrent load against a running instance until it breaks, then
verifies the ledger stayed consistent. See [docs/loadtest.md](docs/loadtest.md).

## Testing

```bash
go test ./...            # unit tests + integration tests (the latter need Docker)
go test -race ./...
```

Integration tests spin up their own disposable PostgreSQL via testcontainers-go.

## Known limitations

- **No performance comparison yet.** Only the row-locking strategy (`SELECT ... FOR
  UPDATE`) exists. A second, `SERIALIZABLE`-with-retry strategy and a measured comparison
  between them are Sessions 7-9. A preliminary, unpolished load test already ran — see the
  "inherited follow-ups" note in Session 7 of the
  [execution plan](docs/plano-execucao-wallet-go.md) for what it found and what is still
  open before it counts as a real measurement.
- **No induced-failure report yet** (Postgres dying mid-load, `SIGTERM` under load,
  `kill -9` mid-transfer). That is Session 13-14.
- **No ADRs written yet.** The decisions exist and are enforced by the code and tests, but
  the formal context/options/consequences write-ups are Session 11.
- **Account creation is not idempotency-key gated.** It relies on the
  `UNIQUE(owner_id, kind, currency)` constraint instead (`409` on repeat), which is a
  narrower guarantee than the `Idempotency-Key` contract used by money-moving endpoints.
- No multi-currency, no real authentication beyond a static API key, no KYC, no scheduled
  or future-dated transfers, no payment system integration — all deliberately out of scope,
  see [docs/escopo-carteira-go.md](docs/escopo-carteira-go.md) §2.
- Balance reads degrade as the number of entries on an account grows, since there is no
  stored balance to read instead.
