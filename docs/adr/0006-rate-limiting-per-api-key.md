# ADR 6 — Rate limiting per API key, default on

## Context

Nothing in this project limited how fast a caller could send requests. The identity
model is already settled for other purposes: exactly one static write key
(`WALLET_API_KEY`) and one optional static read-only key (`WALLET_API_KEY_READONLY`),
both reduced by `apiKeyAuth` to a 2-valued `apiKeyScope` and already reused for
idempotency scoping (`scopeFor`). `WALLET_DB_MAX_CONNS` (default 30) is the real
ceiling on how much concurrent database work the process can do regardless of how
many HTTP requests arrive. `docs/bench/RESULTS.md` records legitimate single-key
throughput up to ~3551 req/s, measured on purpose — any new limit must not make that
unreproducible, only require an explicit, documented step to bypass.

## Options considered

1. **Per source IP.** The usual first instinct for rate limiting, but there is no IP
   concept anywhere in this codebase today, and the threat it addresses (many IPs
   abusing one or many keys) does not fit a system with exactly two static,
   operator-held secrets. Per-IP tracking is also unbounded by nature — it needs
   eviction/TTL machinery this project has no other use for. Declined, the same way
   this project declined its other backlogs on purpose (ADR 5; the two extras queues
   in the README's "Known limitations").
2. **One global limiter**, shared by every caller. Rejected: one noisy key (or a
   client bug in a retry loop) would throttle the *other* key's unrelated traffic too.
3. **Per API-key-scope token bucket (chosen).** Reuses the identity the project
   already has. Since `apiKeyAuth` collapses identity to `scopeWrite`/`scopeRead`
   before this middleware ever runs, the state is a fixed `[2]*rate.Limiter` — no map,
   no key string, no eviction logic.

Algorithm: token bucket (`golang.org/x/time/rate`) over a fixed or sliding window.
Token bucket was chosen because it tolerates a short burst without flagging it as
abuse, and `Limiter.Reserve()` gives an exact wait time for `Retry-After` — a fixed
window would only give a yes/no.

## Decision

Rate limiting is **on by default**: 50 requests/second per API key, burst 100.
`WALLET_RATE_LIMIT_RPS=0` disables it entirely (no middleware is inserted at all, not
just a very high limit).

Reasoning for on-by-default: this project already treats "secure by default" as the
right posture for the shipped binary and compose file, not an opt-in — `WALLET_API_KEY`
is mandatory, the container runs read-only with every Linux capability dropped (X8), and
`WALLET_DB_MAX_CONNS` defaults to a real number, not "unlimited". A rate limiter that
ships off by default is a feature that is never actually protecting anything unless
someone remembers to turn it on, which is a weaker guarantee than the rest of this
project's defaults already give.

Reasoning for 50/100: `WALLET_DB_MAX_CONNS` already caps the process at ~30 concurrent
database operations. A limit well above that (so legitimate bursts are not punished)
but far below the measured legitimate ceiling (482–3551 req/s) stops an accidental,
no-backoff retry loop from monopolizing the pool and starving the other key, without
being mistaken for a real capacity limit. It is deliberately not sized to the
measurements in `docs/bench/RESULTS.md` — reproducing those is expected to need an
explicit override, not something that should happen by accident.

## Consequences

- **Reproducing `docs/bench/RESULTS.md` or running `cmd/loadtest` for a real
  measurement requires `WALLET_RATE_LIMIT_RPS=0` first.** Documented in
  `docs/loadtest.md` and at the top of `docs/bench/RESULTS.md`, not left to be
  discovered through a confusing wall of `429`s.
- **Unauthenticated traffic (bad or missing API key) is not throttled by this at
  all.** The middleware runs after `apiKeyAuth`, so a caller hammering the API with
  invalid keys only ever gets `401`, as before — this feature adds no protection
  against that. Generic unauthenticated abuse protection is an edge/reverse-proxy
  concern, consistent with this project's declared non-goals (no load balancer or
  edge deployment pursued, see the README).
- **Per-scope, not a map.** `[2]*rate.Limiter` is correct only because `apiKeyAuth`
  already reduces identity to 2 values. If this project ever supported more than two
  keys, this exact design would need to become a map (with the eviction question
  Option 1 was declined over) — not a problem today, but the reason this stays simple
  is the identity model, not something inherent to rate limiting itself.
- **A burst of 0 while the limit is enabled is rejected at config load, not allowed to
  silently lock out every request.** `rate.NewLimiter` with burst 0 never has a token
  to hand out; `loadConfig` in `cmd/wallet/main.go` treats that as a startup error.
