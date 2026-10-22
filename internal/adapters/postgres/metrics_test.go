package postgres

import (
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRegisterPoolStats_ReportsMaxConns(t *testing.T) {
	pool := newTestPool(t)
	reg := prometheus.NewRegistry()
	RegisterPoolStats(reg, pool)

	if got := gatherValue(t, reg, "wallet_db_pool_max_conns"); got <= 0 {
		t.Fatalf("wallet_db_pool_max_conns = %v, want > 0", got)
	}
}

func TestRegisterOutboxLag_ReflectsPendingRows(t *testing.T) {
	pool := newTestPool(t)
	reg := prometheus.NewRegistry()
	RegisterOutboxLag(reg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	before := gatherValue(t, reg, "wallet_outbox_pending_events")
	seedOutboxEvent(t, pool, "transfer.completed")
	seedOutboxEvent(t, pool, "transfer.completed")

	after := gatherValue(t, reg, "wallet_outbox_pending_events")
	if got := after - before; got != 2 {
		t.Fatalf("wallet_outbox_pending_events increased by %v, want 2", got)
	}

	age := gatherValue(t, reg, "wallet_outbox_oldest_pending_age_seconds")
	if age < 0 {
		t.Fatalf("wallet_outbox_oldest_pending_age_seconds = %v, want >= 0", age)
	}
}

// gatherValue reads the current value of a single, label-less gauge/counter
// straight from the registry, the way Prometheus itself would on a scrape.
// Used instead of testutil.ToFloat64 because outboxLagCollector is a custom
// prometheus.Collector emitting two series from one Collect call, not a
// single addressable metric.
func gatherValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if len(f.GetMetric()) != 1 {
			t.Fatalf("metric family %s has %d series, want 1", name, len(f.GetMetric()))
		}
		m := f.GetMetric()[0]
		if g := m.GetGauge(); g != nil {
			return g.GetValue()
		}
		if c := m.GetCounter(); c != nil {
			return c.GetValue()
		}
	}
	t.Fatalf("metric family %s not found", name)
	return 0
}
