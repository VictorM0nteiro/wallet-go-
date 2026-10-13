# ADR 5 — No external currency conversion

## Context

`docs/escopo-carteira-go.md` §11's extras queue originally scoped a read-side currency
conversion feature: `GET /accounts/{id}/balance` optionally returning a converted
amount from an external quote API, with a cache, a short timeout, a circuit breaker,
and a stale-but-labeled fallback — explicitly because a job posting that motivated this
project listed resilience against a flaky external dependency as a requirement.

This ADR exists to record a decision *not* to build that, not to describe a resilience
mechanism that exists in the code — it doesn't. "No feature" is as much an
architectural decision as "feature," and this project's own working rules
(`CLAUDE.md`) ask for ADRs to be driven by the author, which includes the ADR for the
thing that was deliberately left out.

## Options considered

1. **Build it**, as originally scoped: an external adapter with cache/timeout/circuit
   breaker/fallback, called only from the read path, never from a transfer's write
   path (the one non-negotiable rule the original scope attached to this feature: a
   converted value is never persisted into `entries`, since it would be wrong the
   moment the rate changes).
2. **Don't build it**, and keep the ledger single-currency.

## Decision

Option 2, decided 2026-10-13. Two separate extras backlogs had accumulated on this
project — this project's own (`docs/escopo-carteira-go.md` §11) and a second one mined
from an unrelated take-home challenge to practice unrelated patterns
(`docs/plano-execucao-wallet-go.md`'s X1-X7). Both were declined in favor of finishing
the core measurement/documentation phases (Sessions 9-12) instead of extending scope —
see the README's "Known limitations" for the full reasoning. Currency conversion was
not special-cased out of that decision: it is in scope exactly as much as the rest of
the backlog, which is to say, not pursued.

## Consequences

- **The ledger stays single-currency.** `entries` carries no currency unit beyond the
  `currency` column already on `accounts`; there is no exchange-rate adapter, no
  circuit breaker, no cache in this codebase.
- **The resilience pattern this ADR would have demonstrated — graceful degradation
  when a third-party dependency is unavailable — is not proven anywhere in this
  project.** If that is specifically what a reader is looking for, it is a gap, not an
  oversight; it is named as one here and in the README.
- **If this is revisited**, the non-negotiable rule from the original scope still
  applies: a converted value is read-side enrichment only, computed on the way out,
  never written to `entries`. Writing it down here means that constraint survives even
  though the feature itself didn't get built.
