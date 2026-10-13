# ADR 2 — Money as `int64` cents, never float or decimal

## Context

Every amount in this system — a transfer, a deposit, a ledger entry, a JSON field — has
to represent currency without ever losing or fabricating a cent, under arithmetic that
runs on every request.

## Options considered

1. **`float64`.** Native, fast, but binary floating point cannot represent most decimal
   fractions exactly. `0.1 + 0.2 != 0.3` in IEEE 754, and that is not a corner case for
   money — it is every other amount. Rounding errors in a ledger are not cosmetic, they
   are the invariant check failing.
2. **A decimal type** (e.g. `shopspring/decimal` or Postgres `numeric`). Exact, but
   allocates on the hot path (arbitrary-precision arithmetic is not a value type) and
   pulls in a dependency for a problem integers already solve for a currency with two
   decimal places.
3. **`int64` cents.** `domain.Money` is a named `int64`. R$ 1,00 is `100`. Addition and
   subtraction are native integer ops, with explicit overflow checks
   (`internal/domain/money.go`).

## Decision

Option 3. `Money` is `int64` cents everywhere in the domain, the database
(`amount_cents BIGINT`), and the wire format (`"amount_cents": 10000` as a JSON
integer). Human formatting (`"R$ 97,00"`) exists only as `Money.String()`, called at
the presentation edge, never via `MarshalJSON` — so a client can never receive a
decimal string where it expects an integer.

## Consequences

- **Zero allocations, nanosecond cost.** Session 9's `go test -bench -benchmem` on the
  domain's hot path (`CanDebit`, `Money.Add`, `NewTransferAmount`) measured 0 B/op, 0
  allocs/op, low single-digit nanoseconds per call — see `docs/performance-report.md`.
  Whatever a transfer's latency is made of, the money arithmetic is not it.
- **Overflow has to be handled explicitly**, since `int64` wraps instead of raising on
  overflow in Go. `Money.Add`/`Sub` check for it and return `domain.ErrMoneyOverflow`
  rather than silently wrapping to a negative or nonsensical value.
- **No multi-currency support**, by construction — `Money` carries no currency unit, and
  the schema's `currency` column exists on `accounts` but is not enforced across a
  transfer's two sides beyond what the existing tests check. Documented in the README's
  "Known limitations."
