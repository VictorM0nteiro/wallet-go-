# ADR 8 — Observability: Prometheus client direct, no OTel SDK

## Context

Nothing in this project exposed what it was doing at runtime beyond `/healthz`,
`/readyz`, and structured logs. `go.mod` already lists several `go.opentelemetry.io/*`
packages, which looks like prior art to build on — it is not: they are indirect
dependencies pulled in only by `testcontainers-go`'s own Docker client
(`moby/moby/client` uses `otelhttp`), confirmed with `go mod why`, and never imported
by this project's own code. There is nothing "OTel-shaped" already wired up here.

## Options considered

1. **Full OpenTelemetry SDK + Collector + Prometheus remote-write.** Rejected: the
   end result for a dashboard is the same set of counters/histograms, at the cost of
   a Collector process and a heavier SDK, for a single-process API that is not
   distributed — there is nothing to trace across services yet (see Consequences).
   The `go.opentelemetry.io/*` entries already in `go.mod` do not change this
   calculus, since none of them are reachable from this project's own code today.
2. **`prometheus/client_golang` direct (chosen).** Leaner, and matches a pattern this
   project already has: `/healthz`/`/readyz` are pull-based (something else asks the
   process for its state), and `/metrics` is the exact same shape — Prometheus pulls,
   nothing is pushed.
3. **No metrics.** Rejected — this is the point of the pass.

## Decision

**Direct `client_golang`, never `prometheus.DefaultRegisterer`.** A fresh
`*prometheus.Registry` is built per `NewRouter` call in `httpapi` (production and
every test) and per `cmd/relay` process, the same non-shared-state rule
`newPerScopeLimiters` already established for rate limiting (ADR 6) — a global
registry would leak counts between independent test runs the same way a
package-level limiter would have leaked buckets.

**Business metrics are incremented at the HTTP-handler layer, not inside
`internal/app`.** `statusFor` (`errors.go`) already centralizes the one
error-to-code mapping this project has; `writeError` increments
`wallet_business_errors_total` right after calling it, reusing that mapping instead
of inventing a second taxonomy inside the use case. `internal/app` gets no new
import.

**The route label is the matched chi pattern, never the raw path.**
`chi.RouteContext(r.Context()).RoutePattern()`, with `"unmatched"` as the fallback for
a literal 404 — otherwise every distinct account UUID, or every path a scanner
invents, would mint its own Prometheus series forever (cardinality explosion, not a
dashboard).

**`/metrics` is unauthenticated, the same precedent as `/healthz`/`/readyz`.**
<!-- author: state here, in your own words, why you picked this over gating it
behind the read-only API key — the tradeoff this project cares about is that
WALLET_API_KEY is mandatory everywhere else (ADR 6's "secure by default"), and this
is the one unauthenticated endpoint that exposes more than a boolean. -->

**`cmd/relay` gets a second, small HTTP server, on its own port
(`RELAY_METRICS_ADDR`, default `:9091`), just for `/metrics`.** This does not reopen
ADR 7's "no HTTP server" framing: that decision was about the relay not needing a
consumer-facing API to do its job (it polls, nothing calls it) — a metrics endpoint
is observability plumbing, not a reversal of that, and does not add the broker
dependency ADR 7 declined. `wallet_relay_published_total{outcome}` and
`wallet_relay_poll_duration_seconds` can only be observed from inside the relay
itself: no other process calls `Publisher.Publish`.

**Outbox lag is read by query at scrape time, not by a background ticker.**
`wallet_outbox_pending_events` and `wallet_outbox_oldest_pending_age_seconds`
(`internal/adapters/postgres/metrics.go`) run one `SELECT count(*), min(created_at)
... WHERE published_at IS NULL` per scrape, via a custom `prometheus.Collector` so
both numbers come from the same query instead of two independent `GaugeFunc`s
re-querying the same row. Since Prometheus pulls, there is nothing to poll on a
clock the way `cmd/relay`'s own delivery loop does — the gauge is only ever as fresh
as the last scrape, which is the correct trade for a metric, unlike delivery itself.

**This metric is registered in `cmd/relay`'s registry, not `cmd/wallet`'s — not a
free choice.** The first attempt put it in `cmd/wallet`, reasoning that "any process
holding a `*postgres.Pool` can run this query." That reasoning ignored X8's own
least-privilege grants (migration `000003_outbox.up.sql`): `wallet_app`, the role
`cmd/wallet` connects as, has `INSERT` on `outbox` only, never `SELECT` —
`AppendEvent` writes, nothing in that process reads. The query failed on every
scrape, silently, because a `prometheus.Collector.Collect` has no way to propagate
an error to its caller — it was only caught by actually running `docker compose up`
and checking `/metrics` output, not by the unit tests (`metrics_test.go`'s
`newTestPool` connects with elevated privileges, the same way the rest of this
package's tests do, so it never exercised the real per-role restriction).
`wallet_relay` already has `SELECT` on `outbox` for its own polling, so the
collector moved to `cmd/relay`'s registry, where it belongs on privilege grounds,
not just convenience. `outboxLagCollector.Collect` also logs the query error now
(`*slog.Logger`, threaded through `RegisterOutboxLag`) — the failure mode above is
exactly the kind of thing that must not fail silently forever.

## Consequences

- **No distributed tracing in this pass.** A single-process API plus one polling
  relay reading the same table is not a system with a request that crosses multiple
  services — there is no span graph worth building. If a second consumer of the
  outbox is ever written (see the README's "known limitations" on `LogPublisher`),
  tracing a request across that hop would be the actual argument for OpenTelemetry,
  not this pass.
- **No alerting rules, no Alertmanager.** Prometheus here is a metrics store and
  Grafana a dashboard, nothing is wired to page anyone — `docs/resilience-report.md`
  already covers what this project knows about failure behavior through
  induced-failure testing, not through automated alerts.
- **`/metrics` composition is deliberately free of account data.** Every label is a
  route pattern, an HTTP method, a status code, an error code, or an outcome string
  — never an account ID, owner ID, or amount. This is what makes the unauthenticated
  decision above defensible: the payload is operational counts and histograms, not a
  narrower version of what the API key already protects.
- **Two registries, two ports, by design.** `cmd/wallet` and `cmd/relay` are separate
  binaries with separate `/metrics` (`:8080` and `:9091` respectively) and separate
  Prometheus scrape jobs (`deploy/prometheus/prometheus.yml`) — there is no shared
  registry to accidentally double-register a metric name across both processes.
