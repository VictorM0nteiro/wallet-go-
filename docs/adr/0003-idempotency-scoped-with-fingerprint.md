# ADR 3 — Idempotency scoped by `(scope, endpoint, key)`, with a stored fingerprint

## Context

A client retrying a `POST /transfers` after a timeout must not move money twice, and
must get back the same response it would have gotten the first time. The
`Idempotency-Key` header is the client-supplied identifier for "this is the same
logical request."

## Options considered

1. **Global key uniqueness** — a table keyed only by the client's string. Simple, but
   two different API keys ("scopes") can pick the same string by coincidence, and one
   caller's retry would collide with an unrelated caller's request.
2. **Key without a fingerprint** — scoped, but the same key with a *different* request
   body (a bug on the client, or two unrelated requests that reused a key) would replay
   whatever was stored the first time, silently serving the wrong response for the
   second request.
3. **Key scoped by `(scope, endpoint, key)`, plus a stored SHA-256 fingerprint of the
   canonicalized request body.** Scope comes from the API key
   (`internal/adapters/http/router.go`'s `scopeFor`), so two callers never collide.
   Endpoint is part of the key so the same string can't be replayed across different
   operations. The fingerprint (`app.Fingerprint`, `internal/app/idempotency.go`) is a
   hash of the JSON body with map keys sorted and numbers normalized — so semantically
   identical bodies fingerprint the same, byte-different bodies don't.

## Decision

Option 3. `idempotency_keys` is primary-keyed on `(scope, endpoint, key)`. A request
claims the key (`INSERT ... ON CONFLICT DO NOTHING`), and if the key already exists:
the stored fingerprint is compared against the incoming request's; a match replays the
stored response with `Idempotent-Replayed: true`, a mismatch is rejected with `422
idempotency_key_reuse`. The claim, the transfer, and storing the response happen in one
database transaction — they commit or roll back together, which is what makes "exactly
once" hold under concurrent retries, not just sequential ones.

## Consequences

- **Idempotency logic lives in `internal/app/`, not the HTTP handler.** The handler only
  extracts the header and the body; claim/replay/reject is a use-case concern, testable
  without an HTTP server.
- **`ConcurrentSameKeyTransfersExactlyOnce` is a real concurrency test**
  (`internal/adapters/postgres/idempotency_test.go`), not just a unit test of the
  fingerprint function — it fires the same key from multiple goroutines and checks
  exactly one transfer was created.
- **A reused key with a different body is a client bug, surfaced, not hidden.** The
  `422` forces the caller to notice instead of silently getting an unrelated stored
  response back.
- **Account creation does not get this guarantee** — it relies on
  `UNIQUE(owner_id, kind, currency)` instead (`409` on repeat), a narrower guarantee than
  the `Idempotency-Key` contract. Documented in the README's "Known limitations."
- **This guarantees "exactly once" for a deterministic failure too, but not a stable
  response for one.** Claim and execution are the same transaction, so if the transfer
  fails for a reason that doesn't depend on timing (`insufficient_funds`, an unknown
  account), the rollback undoes the claim along with it. A retry with that key runs
  again from scratch rather than replaying the original error — the opposite of how,
  say, Stripe's idempotency keys behave, where an error response is stored and
  replayed like a success would be. This project accepted that trade-off as a direct
  consequence of the property it did want (claim and execution committing or rolling
  back together); it was not a separate decision.
- **`409 request_in_flight` is reachable in principle, almost unobservable in
  practice.** `ClaimKey`'s `INSERT ... ON CONFLICT DO NOTHING` is shared by both
  strategies. If a second request with the same key arrives while the first's claim is
  still uncommitted, Postgres makes that `INSERT` wait for the first transaction's
  outcome before resolving the conflict — this is documented Postgres behavior for
  `ON CONFLICT` specifically, not a side effect of either concurrency strategy. Once it
  resolves: if the first committed, the second finds `state = completed` and replays;
  if the first rolled back, the row is gone and the second claims fresh. Either way,
  the second request's own `ClaimKey` call simply takes longer — it does not observe
  `in_flight` and does not get `409`. `TestIdempotency_InFlightKeyIsRejected`
  (`internal/adapters/postgres/idempotency_test.go`) exercises the rejection itself, but
  it does so by inserting an `in_flight` row directly with raw SQL before calling
  `Transfer`, not by racing two real requests — because claim, execution and
  `CompleteKey` all commit as one transaction, no code path in this project today
  leaves a key committed as `in_flight` for a second request to actually find. The
  check is real and correct; the condition it guards against may not be reachable from
  the HTTP API as currently built.
