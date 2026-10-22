package postgres

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RelayMetrics holds the counters/histogram one cmd/relay process registers
// into its own registry -- never package-level state, same rule every other
// metrics constructor in this project follows.
type RelayMetrics struct {
	published *prometheus.CounterVec
	pollTime  prometheus.Histogram
}

// NewRelayMetrics builds and registers the relay's own metrics into reg.
func NewRelayMetrics(reg *prometheus.Registry) *RelayMetrics {
	m := &RelayMetrics{
		published: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_relay_published_total",
			Help: "Outbox rows the relay attempted to publish, by outcome (ok or failed).",
		}, []string{"outcome"}),
		pollTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "wallet_relay_poll_duration_seconds",
			Help:    "Duration of one PollOnce call.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	reg.MustRegister(m.published, m.pollTime)
	return m
}

func (m *RelayMetrics) observePoll(d time.Duration) {
	if m == nil {
		return
	}
	m.pollTime.Observe(d.Seconds())
}

func (m *RelayMetrics) recordOutcome(published bool) {
	if m == nil {
		return
	}
	outcome := "failed"
	if published {
		outcome = "ok"
	}
	m.published.WithLabelValues(outcome).Inc()
}

// ============================================================================
// COMO ESSE ARQUIVO FUNCIONA (anotações de estudo)
// ============================================================================
//
// 1. Por que isso existe separado de metrics.go
//
// metrics.go tem as métricas que qualquer processo com acesso ao *Pool
// já teria como obter (stats do pgxpool, lag do outbox por query). As
// métricas deste arquivo só fazem sentido do ponto de vista do relay
// especificamente: "quantos Publish tiveram sucesso ou falharam" e "quanto
// tempo um PollOnce leva" só existem porque o relay é quem chama Publish.
// Por isso cmd/relay/main.go monta o SEU PRÓPRIO *prometheus.Registry,
// separado do registry do cmd/wallet — são dois binários distintos, cada
// um com sua própria porta de métricas (:8080/metrics e :9091/metrics),
// nunca a mesma instância de registry.
//
// 2. receiver nil-safe (if m == nil) — por quê
//
// NewOutboxRelay(pool) continua funcionando sem .WithMetrics(...) — todo
// teste e código já existente antes desta mudança (relay_test.go) chama só
// NewOutboxRelay(pool), sem metrics nenhum. Em Go, chamar um método num
// ponteiro nil é seguro desde que o método não desreferencie esse ponteiro
// (m.algumCampo) sem checar antes — por isso observePoll e recordOutcome
// começam com "if m == nil { return }": OutboxRelay.metrics fica nil por
// padrão, PollOnce chama r.metrics.observePoll(...) e r.metrics.recordOutcome(...)
// de qualquer forma, e nesses casos o método simplesmente não faz nada, em
// vez de precisar de um "if r.metrics != nil" repetido toda vez em relay.go.
//
// 3. outcome ∈ {"ok", "failed"} num único CounterVec, em vez de dois Counters
//
// Poderia existir um wallet_relay_published_total e um
// wallet_relay_failed_total separados. Usar um CounterVec com o label
// "outcome" em vez disso deixa as duas séries juntas sob o mesmo nome de
// métrica no Grafana/Prometheus — uma query como
// sum(rate(wallet_relay_published_total[5m])) by (outcome) já separa sucesso
// de falha automaticamente, sem precisar somar duas métricas diferentes à
// mão.
