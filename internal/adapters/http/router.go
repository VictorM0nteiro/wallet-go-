package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// Deps are the collaborators of the HTTP adapter.
type Deps struct {
	Accounts       accountService
	Cash           cashService
	Transfers      transferService
	DB             Pinger
	APIKey         string
	ReadOnlyAPIKey string
	// RateLimitRPS <= 0 disables rate limiting entirely: no middleware is
	// inserted, zero overhead. See docs/adr/0006-rate-limiting-per-api-key.md.
	RateLimitRPS   float64
	RateLimitBurst int
	RequestTimeout time.Duration
	Logger         *slog.Logger
	// Registry collects this process's Prometheus metrics. Built fresh per
	// NewRouter call (production and every test), never a shared/global
	// registry. See docs/adr/0008-prometheus-metrics-direct-client.md.
	Registry *prometheus.Registry
}

// NewRouter wires the six endpoints, health checks, metrics, and the
// middleware chain.
func NewRouter(d Deps) http.Handler {
	if d.Registry == nil {
		// Every existing test builds Deps{} without a Registry; defaulting
		// here means only the metrics-specific tests need to pass one.
		d.Registry = NewRegistry()
	}
	metrics := newHTTPMetrics(d.Registry)
	h := &Handler{
		accounts:  d.Accounts,
		cash:      d.Cash,
		transfers: d.Transfers,
		scope:     scopeFor(d.APIKey),
		logger:    d.Logger,
		metrics:   metrics,
	}

	r := chi.NewRouter()
	r.Use(requestID, accessLog(d.Logger), recoverer(d.Logger), timeout(d.RequestTimeout), httpMetrics(metrics))

	// Health checks and metrics are unauthenticated: the orchestrator
	// polling health has no API key, and Prometheus's own scrape model has
	// no simple way to carry one either. None of the three leak account
	// data -- only process liveness and operational counts/histograms.
	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(d.DB))
	r.Get("/metrics", metricsHandler(d.Registry))

	r.Group(func(r chi.Router) {
		r.Use(apiKeyAuth(d.APIKey, d.ReadOnlyAPIKey))
		if d.RateLimitRPS > 0 {
			r.Use(rateLimit(newPerScopeLimiters(d.RateLimitRPS, d.RateLimitBurst)))
		}
		r.Get("/accounts/{id}/balance", h.balance)
		r.Get("/accounts/{id}/entries", h.entries)

		r.Group(func(r chi.Router) {
			r.Use(requireWriteScope)
			r.Post("/accounts", h.createAccount)
			r.Post("/accounts/{id}/deposits", h.deposit)
			r.Post("/accounts/{id}/withdrawals", h.withdraw)
			r.Post("/transfers", h.transfer)
		})
	})
	return r
}

// scopeFor derives the idempotency scope (the owner of a key) from the API
// key without ever storing the key itself.
func scopeFor(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return "apikey:" + hex.EncodeToString(sum[:8])
}
