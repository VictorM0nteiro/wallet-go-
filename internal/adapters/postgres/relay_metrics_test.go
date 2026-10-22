package postgres

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRelayMetrics_RecordsPublishedAndFailedOutcomes(t *testing.T) {
	pool := newTestPool(t)
	reg := prometheus.NewRegistry()
	metrics := NewRelayMetrics(reg)
	relay := NewOutboxRelay(pool).WithMetrics(metrics)

	seedOutboxEvent(t, pool, "transfer.completed")
	bad := seedOutboxEvent(t, pool, "transfer.completed")

	pub := &funcPublisher{fn: func(e Event) error {
		if e.ID == bad {
			return errBoom
		}
		return nil
	}}
	if _, err := relay.PollOnce(context.Background(), pub, 2); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if got := gatherLabeledValue(t, reg, "wallet_relay_published_total", "outcome", "ok"); got != 1 {
		t.Errorf("wallet_relay_published_total{outcome=\"ok\"} = %v, want 1", got)
	}
	if got := gatherLabeledValue(t, reg, "wallet_relay_published_total", "outcome", "failed"); got != 1 {
		t.Errorf("wallet_relay_published_total{outcome=\"failed\"} = %v, want 1", got)
	}
	if got := gatherHistogramCount(t, reg, "wallet_relay_poll_duration_seconds"); got != 1 {
		t.Errorf("wallet_relay_poll_duration_seconds sample count = %v, want 1 (one PollOnce call)", got)
	}
}

func gatherHistogramCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
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
		return f.GetMetric()[0].GetHistogram().GetSampleCount()
	}
	t.Fatalf("metric family %s not found", name)
	return 0
}

// TestOutboxRelay_NilMetricsIsSafe guards the WithMetrics-is-optional
// contract every existing caller of NewOutboxRelay relies on.
func TestOutboxRelay_NilMetricsIsSafe(t *testing.T) {
	pool := newTestPool(t)
	relay := NewOutboxRelay(pool)
	seedOutboxEvent(t, pool, "transfer.completed")

	if _, err := relay.PollOnce(context.Background(), &funcPublisher{}, 1); err != nil {
		t.Fatalf("PollOnce with no metrics attached: %v", err)
	}
}

func gatherLabeledValue(t *testing.T, reg *prometheus.Registry, name, labelName, labelValue string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelName && lp.GetValue() == labelValue {
					if c := m.GetCounter(); c != nil {
						return c.GetValue()
					}
				}
			}
		}
	}
	t.Fatalf("metric %s{%s=%q} not found", name, labelName, labelValue)
	return 0
}
