package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Deps are the collaborators of the HTTP adapter.
type Deps struct {
	Accounts       accountService
	Cash           cashService
	Transfers      transferService
	APIKey         string
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

// NewRouter wires the six endpoints and the middleware chain.
func NewRouter(d Deps) http.Handler {
	h := &Handler{
		accounts:  d.Accounts,
		cash:      d.Cash,
		transfers: d.Transfers,
		scope:     scopeFor(d.APIKey),
		logger:    d.Logger,
	}

	r := chi.NewRouter()
	r.Use(requestID, accessLog(d.Logger), recoverer(d.Logger), timeout(d.RequestTimeout))
	r.Group(func(r chi.Router) {
		r.Use(apiKeyAuth(d.APIKey))
		r.Post("/accounts", h.createAccount)
		r.Post("/accounts/{id}/deposits", h.deposit)
		r.Post("/accounts/{id}/withdrawals", h.withdraw)
		r.Post("/transfers", h.transfer)
		r.Get("/accounts/{id}/balance", h.balance)
		r.Get("/accounts/{id}/entries", h.entries)
	})
	return r
}

// scopeFor derives the idempotency scope (the owner of a key) from the API
// key without ever storing the key itself.
func scopeFor(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return "apikey:" + hex.EncodeToString(sum[:8])
}
