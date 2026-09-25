# Running wallet-go locally

Step-by-step guide to run the API on your machine, from an empty checkout to a
working transfer.

## Prerequisites

- **Go 1.25+**
- **Docker Desktop**, running (used for PostgreSQL and for the integration tests)
- A bash-compatible shell for the `curl` examples. On Windows use **Git Bash**:
  PowerShell 5.1 mangles the quotes inside JSON bodies.
- Optional, Windows only: **mingw-w64** (64-bit gcc), required by `go test -race`.

## 1. Configure the environment

The app reads its configuration from environment variables. For local
development they are loaded from a `.env` file in the working directory.

```bash
cp .env.example .env
```

| Variable         | Required | Default | Description                                   |
|------------------|----------|---------|-----------------------------------------------|
| `DATABASE_URL`   | yes      |         | PostgreSQL connection string                  |
| `WALLET_API_KEY` | yes      |         | Static API key expected in the `X-API-Key` header |
| `LISTEN_ADDR`    | no       | `:8080` | Address the HTTP server listens on            |

The `.env` file is git-ignored. Always run the app from the repository root,
because that is where `.env` is looked up.

## 2. Start PostgreSQL

```bash
docker compose up -d
docker compose ps
```

Wait until `wallet-go-postgres-1` shows `healthy`.

## 3. Apply the database migrations

```bash
go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate \
  -path migrations \
  -database "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable" \
  up
```

- `1/u init_schema` means the schema was just created.
- `no change` means it was already applied. Data survives container restarts
  because it lives in a named Docker volume.

The `-tags postgres` flag is required: without it the Postgres driver is not
registered and the command fails with `unknown driver postgres`.

## 4. Run the API

```bash
go run ./cmd/wallet
```

You should see a log line like:

```json
{"level":"INFO","msg":"wallet: listening","addr":":8080"}
```

The process stays in the foreground. Leave this terminal open and use a second
one for the next step. On startup the app creates the single `system` account
if it does not exist yet (it is the counterparty of deposits and withdrawals).

## 5. Try the API

Rules to know before calling it:

- Every request needs the `X-API-Key` header.
- Deposits, withdrawals and transfers also need an `Idempotency-Key` header.
  Sending the same key again with the same body replays the original response
  (header `Idempotent-Replayed: true`) without moving money twice. Sending the
  same key with a different body returns `422`.
- Money is always an integer number of cents (`amount_cents`), never a decimal.

```bash
H='-H X-API-Key:dev-key -H Content-Type:application/json'

# Create two accounts
ANA=$(curl -s $H -d '{"owner_id":"ana"}'   localhost:8080/accounts | sed 's/.*"id":"\([^"]*\)".*/\1/')
BRU=$(curl -s $H -d '{"owner_id":"bruno"}' localhost:8080/accounts | sed 's/.*"id":"\([^"]*\)".*/\1/')

# Deposit R$ 100.00 into Ana's account
curl -s $H -H 'Idempotency-Key: dep-1' -d '{"amount_cents":10000}' localhost:8080/accounts/$ANA/deposits

# Repeat the same request: same response, header Idempotent-Replayed: true, no second deposit
curl -s -i $H -H 'Idempotency-Key: dep-1' -d '{"amount_cents":10000}' localhost:8080/accounts/$ANA/deposits

# Transfer R$ 30.00 from Ana to Bruno
curl -s $H -H 'Idempotency-Key: tr-1' \
  -d "{\"from_account_id\":\"$ANA\",\"to_account_id\":\"$BRU\",\"amount_cents\":3000}" \
  localhost:8080/transfers

# Balances are derived from the ledger: 7000 and 3000
curl -s $H localhost:8080/accounts/$ANA/balance
curl -s $H localhost:8080/accounts/$BRU/balance

# Statement, cursor-paginated (pass next_after as ?after= to get the next page)
curl -s $H "localhost:8080/accounts/$ANA/entries?limit=1"
```

### Endpoints

| Method | Path                            | Idempotency-Key | Success |
|--------|---------------------------------|-----------------|---------|
| POST   | `/accounts`                     | no              | `201`   |
| POST   | `/accounts/{id}/deposits`       | required        | `201`   |
| POST   | `/accounts/{id}/withdrawals`    | required        | `201`   |
| POST   | `/transfers`                    | required        | `201`   |
| GET    | `/accounts/{id}/balance`        | no              | `200`   |
| GET    | `/accounts/{id}/entries`        | no              | `200`   |

`GET /accounts/{id}/entries` accepts `after` (entry id cursor, default `0`) and
`limit` (1 to 200, default 50).

### Errors

Errors are returned as `{"error":{"code":"...","message":"..."}}`.

| Status | Code                                                                              |
|--------|-----------------------------------------------------------------------------------|
| 400    | `bad_request` (invalid JSON, unknown field, missing `Idempotency-Key`, bad UUID)  |
| 401    | `unauthorized`                                                                    |
| 404    | `account_not_found`                                                               |
| 409    | `account_already_exists`, `request_in_flight`                                     |
| 422    | `invalid_amount`, `same_account`, `insufficient_funds`, `idempotency_key_reuse`, `invalid_owner`, `amount_overflow` |
| 503    | `service_unavailable` (request or database timeout)                               |
| 500    | `internal_error`                                                                  |

## 6. Run the tests

```bash
go test ./...            # unit and integration tests (integration needs Docker)
go test -race ./...      # same, with the race detector
```

The integration tests start their own throwaway PostgreSQL containers through
testcontainers, so they do not touch the database from step 2.

Idempotency under concurrency, repeated to rule out flakiness:

```bash
go test -race -count=20 -run ConcurrentSameKey ./internal/adapters/postgres/
```

## 7. Stop and clean up

- Stop the API: `Ctrl+C` in its terminal.
- Stop PostgreSQL and keep the data: `docker compose down`
- Stop PostgreSQL and delete the data: `docker compose down -v`
  (run the migrations again on the next start)

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `failed to connect to the docker API` | Docker Desktop is not running. Start it and retry. |
| `connectex: ... actively refused it` on port 5432 | The PostgreSQL container is not up. Run `docker compose up -d` and check `docker compose ps`. |
| `bind: Only one usage of each socket address` | Port 8080 is taken by another process. Stop it or set `LISTEN_ADDR` in `.env`. |
| `DATABASE_URL and WALLET_API_KEY must be set` | `.env` is missing or you are not in the repository root. |
| `unknown driver postgres` when migrating | The `-tags postgres` flag is missing. |
| `401 unauthorized` | The `X-API-Key` header does not match `WALLET_API_KEY`. |
| `64-bit mode not compiled in` on `go test -race` (Windows) | An old 32-bit MinGW is first on `PATH`. Install a 64-bit mingw-w64 (for example `winget install BrechtSanders.WinLibs.POSIX.UCRT`) and point Go at it with `go env -w CC=<path to gcc.exe>`. |
