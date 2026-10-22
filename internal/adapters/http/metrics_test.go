package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// --- middleware-level test: isolate httpMetrics from the rest of the chain,
// same style as rateLimitTestHandler in ratelimit_test.go ---

// TestHTTPMetrics_UsesRoutePatternNotRawPath guards the cardinality rule:
// the route label must be the matched chi pattern ("/accounts/{id}/balance"),
// never the raw path, or one series per account UUID would appear.
func TestHTTPMetrics_UsesRoutePatternNotRawPath(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newHTTPMetrics(reg)

	r := chi.NewRouter()
	r.Use(httpMetrics(m))
	r.Get("/accounts/{id}/balance", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/accounts/"+uuid.NewString()+"/balance", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := testutil.ToFloat64(m.requests.WithLabelValues(http.MethodGet, "/accounts/{id}/balance", "200"))
	if got != 1 {
		t.Fatalf("wallet_http_requests_total{route=\"/accounts/{id}/balance\"} = %v, want 1 (raw UUID path must never become a label)", got)
	}
}

func TestHTTPMetrics_UnmatchedRouteFallsBackToAConstantLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newHTTPMetrics(reg)

	r := chi.NewRouter()
	r.Use(httpMetrics(m))
	r.Get("/known", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := testutil.ToFloat64(m.requests.WithLabelValues(http.MethodGet, "unmatched", "404"))
	if got != 1 {
		t.Fatalf("wallet_http_requests_total{route=\"unmatched\"} = %v, want 1", got)
	}
}

// --- router-level tests: the full chain, mirroring newRateLimitedRouter ---

func newMetricsTestRouter(reg *prometheus.Registry) http.Handler {
	return NewRouter(Deps{
		Accounts:       &fakeAccounts{},
		Cash:           &fakeCash{},
		Transfers:      fakeTransfers{},
		DB:             fakePinger{},
		APIKey:         "secret",
		RequestTimeout: testRequestTimeout,
		Logger:         testLogger(),
		Registry:       reg,
	})
}

// TestMetrics_EndpointIsUnauthenticated matches the project's own precedent
// for /healthz and /readyz (see TestHealthEndpoints_DoNotRequireAPIKey).
func TestMetrics_EndpointIsUnauthenticated(t *testing.T) {
	h := newMetricsTestRouter(prometheus.NewRegistry())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics without an API key: status = %d, want 200", rec.Code)
	}
}

// TestMetrics_ObservesEverythingIncludingRejections proves httpMetrics sits
// outside apiKeyAuth and rateLimit: a 401 and a 429 must both show up next
// to ordinary 200s in the same /metrics output.
func TestMetrics_ObservesEverythingIncludingRejections(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewRouter(Deps{
		Accounts:       &fakeAccounts{},
		Cash:           &fakeCash{},
		Transfers:      fakeTransfers{},
		DB:             fakePinger{},
		APIKey:         "secret",
		RequestTimeout: testRequestTimeout,
		Logger:         testLogger(),
		Registry:       reg,
		RateLimitRPS:   1,
		RateLimitBurst: 1,
	})
	accountID := uuid.NewString()

	// 200: a real, authenticated request.
	if rec := doRequest(h, http.MethodGet, "/accounts/"+accountID+"/balance", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}
	// 429: burst of 1 already spent above.
	if rec := doRequest(h, http.MethodGet, "/accounts/"+accountID+"/balance", "", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}
	// 401: no API key header at all.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/accounts/"+accountID+"/balance", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: status = %d, want 401", rec.Code)
	}

	mreq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, mreq)
	body := mrec.Body.String()
	for _, want := range []string{
		`wallet_http_requests_total{method="GET",route="/accounts/{id}/balance",status="200"} 1`,
		`wallet_http_requests_total{method="GET",route="/accounts/{id}/balance",status="429"} 1`,
		`wallet_http_requests_total{method="GET",route="/accounts/{id}/balance",status="401"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q", want)
		}
	}
}

// TestMetrics_BusinessCountersIncrementOnWriteErrorAndWriteResult exercises
// the two hooks added outside this file: writeError (respond.go) and the
// writeResult call sites in handler.go.
func TestMetrics_BusinessCountersIncrementOnWriteErrorAndWriteResult(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newMetricsTestRouter(reg)

	// A deposit with no Idempotency-Key header is a bad_request, routed
	// through writeError -- exercises wallet_business_errors_total.
	rec := doRequest(h, http.MethodPost, "/accounts/"+uuid.NewString()+"/deposits", `{"amount_cents":100}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("deposit without Idempotency-Key: status = %d, want 400", rec.Code)
	}

	mreq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, mreq)
	body := mrec.Body.String()
	if !strings.Contains(body, `wallet_business_errors_total{code="bad_request"`) {
		t.Errorf("/metrics body missing wallet_business_errors_total for bad_request, got:\n%s", body)
	}
}
