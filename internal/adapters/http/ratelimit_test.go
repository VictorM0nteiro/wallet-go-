package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// --- middleware-level tests: bypass apiKeyAuth, inject the scope by hand ---

func rateLimitTestHandler(limiters perScopeLimiters) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return rateLimit(limiters)(next)
}

func doScopedRequest(h http.Handler, scope apiKeyScope) *httptest.ResponseRecorder {
	ctx := context.WithValue(context.Background(), apiKeyScopeKey, scope)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRateLimit_BurstThenReject(t *testing.T) {
	h := rateLimitTestHandler(newPerScopeLimiters(1, 2)) // 1 req/s, burst 2

	for i := range 2 {
		rec := doScopedRequest(h, scopeWrite)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (within burst)", i+1, rec.Code)
		}
	}

	rec := doScopedRequest(h, scopeWrite)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the burst is exhausted", rec.Code)
	}
	if code := errorCode(t, rec); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("missing Retry-After header on a 429")
	}
}

func TestRateLimit_ScopesAreIndependent(t *testing.T) {
	h := rateLimitTestHandler(newPerScopeLimiters(1, 1)) // burst 1 each

	if rec := doScopedRequest(h, scopeWrite); rec.Code != http.StatusOK {
		t.Fatalf("write scope, request 1: status = %d, want 200", rec.Code)
	}
	if rec := doScopedRequest(h, scopeWrite); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("write scope, request 2: status = %d, want 429 (burst exhausted)", rec.Code)
	}
	// scopeRead has never been touched: its own bucket is still full.
	if rec := doScopedRequest(h, scopeRead); rec.Code != http.StatusOK {
		t.Fatalf("read scope: status = %d, want 200 (independent bucket from write)", rec.Code)
	}
}

// --- router-level tests: the full chain, same style as TestReadOnlyAPIKey ---

func newRateLimitedRouter(rps float64, burst int) http.Handler {
	return NewRouter(Deps{
		Accounts:       &fakeAccounts{},
		Cash:           &fakeCash{},
		Transfers:      fakeTransfers{},
		DB:             fakePinger{},
		APIKey:         "secret",
		RateLimitRPS:   rps,
		RateLimitBurst: burst,
		RequestTimeout: testRequestTimeout,
		Logger:         testLogger(),
	})
}

func TestRateLimit_DisabledWhenRPSIsZero(t *testing.T) {
	h := newRateLimitedRouter(0, 0)
	accountID := uuid.NewString()

	for i := range 50 {
		rec := doRequest(h, http.MethodGet, "/accounts/"+accountID+"/balance", "", nil)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: got 429 with RateLimitRPS 0, want it never to rate-limit", i+1)
		}
	}
}

func TestRateLimit_EnforcedThroughTheRouter(t *testing.T) {
	h := newRateLimitedRouter(1, 2)
	accountID := uuid.NewString()

	for i := range 2 {
		rec := doRequest(h, http.MethodGet, "/accounts/"+accountID+"/balance", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (within burst)", i+1, rec.Code)
		}
	}
	rec := doRequest(h, http.MethodGet, "/accounts/"+accountID+"/balance", "", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 past the burst", rec.Code)
	}
	if code := errorCode(t, rec); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
}

// TestRateLimit_NewRouterCallsDoNotShareState guards against the limiter
// state ever becoming package-level by accident: every NewRouter call must
// start with its own fresh, full buckets.
func TestRateLimit_NewRouterCallsDoNotShareState(t *testing.T) {
	accountID := uuid.NewString()

	h1 := newRateLimitedRouter(1, 1)
	if rec := doRequest(h1, http.MethodGet, "/accounts/"+accountID+"/balance", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("h1 first request: status = %d, want 200", rec.Code)
	}

	h2 := newRateLimitedRouter(1, 1)
	if rec := doRequest(h2, http.MethodGet, "/accounts/"+accountID+"/balance", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("h2 first request: status = %d, want 200 (a fresh router, independent of h1's exhausted bucket)", rec.Code)
	}
}
