package httpapi

import (
	"context"
	"net/http"
	"time"
)

// Pinger is satisfied by the connection pool. It is the only thing
// readiness depends on.
type Pinger interface {
	Ping(ctx context.Context) error
}

const readyTimeout = 2 * time.Second

// healthz reports whether the process itself is alive. It must never depend
// on an external system — a flaky database would otherwise take down a
// container that is, in fact, fine. See docs/plano-execucao-wallet-go.md,
// Fase E roteiro 1.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether the process can currently serve traffic, i.e.
// whether the database is reachable.
func readyz(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
