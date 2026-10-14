package httpapi

import (
	"net/http"
	"strconv"

	"golang.org/x/time/rate"
)

// perScopeLimiters holds one token bucket per apiKeyScope. apiKeyAuth
// already reduces identity to that 2-valued enum before this middleware
// ever runs, so a fixed array indexed by the scope is enough — no map, no
// extra locking beyond what *rate.Limiter already does internally (it is
// safe for concurrent use on its own).
//
// This must never be a package-level variable: every call to NewRouter
// (production, and every test that builds its own router) needs its own
// limiters, or state would leak between otherwise-independent routers and
// test cases. newPerScopeLimiters is called once per NewRouter, and the
// result is closed over by the middleware it returns.
type perScopeLimiters [2]*rate.Limiter

// newPerScopeLimiters builds one token bucket per scope, rps requests per
// second with the given burst capacity.
func newPerScopeLimiters(rps float64, burst int) perScopeLimiters {
	return perScopeLimiters{
		scopeWrite: rate.NewLimiter(rate.Limit(rps), burst),
		scopeRead:  rate.NewLimiter(rate.Limit(rps), burst),
	}
}

// rateLimit enforces a per-API-key token bucket. It must run after
// apiKeyAuth, which is what puts apiKeyScope in the request context.
//
// Rejection uses Reserve, not Allow: Allow only answers yes/no, Reserve
// also reports how long the caller should wait, which becomes the
// Retry-After header. A rejected reservation is cancelled immediately
// (Cancel) so it does not consume a future token that was never granted.
func rateLimit(limiters perScopeLimiters) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope, _ := r.Context().Value(apiKeyScopeKey).(apiKeyScope)
			limiter := limiters[scope]

			res := limiter.Reserve()
			if !res.OK() {
				// Burst requested is larger than the bucket can ever hold —
				// a config problem, not a client problem. Fail open rather
				// than reject every request forever.
				next.ServeHTTP(w, r)
				return
			}
			if delay := res.Delay(); delay > 0 {
				res.Cancel()
				seconds := int(delay.Seconds()) + 1
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeJSON(w, http.StatusTooManyRequests, errorResponse{
					Error: errorBody{Code: "rate_limited", Message: "rate limit exceeded, retry later"},
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
