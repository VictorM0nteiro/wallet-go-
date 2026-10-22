package postgres

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RegisterPoolStats exposes pool.Stat() as gauges. Cheap: pgxpool already
// tracks these counters internally, this just reads them at scrape time --
// no new goroutine, no new query.
func RegisterPoolStats(reg *prometheus.Registry, pool *Pool) {
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "wallet_db_pool_acquired_conns",
		Help: "Connections currently acquired (in use) from the pool.",
	}, func() float64 { return float64(pool.Stat().AcquiredConns()) }))

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "wallet_db_pool_idle_conns",
		Help: "Connections currently idle in the pool.",
	}, func() float64 { return float64(pool.Stat().IdleConns()) }))

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "wallet_db_pool_max_conns",
		Help: "Maximum connections the pool is configured to hold.",
	}, func() float64 { return float64(pool.Stat().MaxConns()) }))

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "wallet_db_pool_acquire_count",
		Help: "Cumulative number of successful connection acquisitions.",
	}, func() float64 { return float64(pool.Stat().AcquireCount()) }))
}

// outboxLagQueryTimeout bounds the lag query so a stuck database cannot
// hang a Prometheus scrape -- the scrape itself has its own timeout, but
// this keeps the collector from being the thing that blocks forever.
const outboxLagQueryTimeout = 2 * time.Second

// outboxLagCollector runs one query per scrape and emits both lag metrics
// from it, instead of two independent GaugeFuncs each re-querying the same
// row -- Prometheus calls Collect once per scrape regardless of how many
// metrics a Collector emits.
type outboxLagCollector struct {
	pool            *Pool
	logger          *slog.Logger
	pending         *prometheus.Desc
	oldestAgeSecond *prometheus.Desc
}

func newOutboxLagCollector(pool *Pool, logger *slog.Logger) *outboxLagCollector {
	return &outboxLagCollector{
		pool:   pool,
		logger: logger,
		pending: prometheus.NewDesc(
			"wallet_outbox_pending_events", "Outbox rows not yet published.", nil, nil),
		oldestAgeSecond: prometheus.NewDesc(
			"wallet_outbox_oldest_pending_age_seconds",
			"Age in seconds of the oldest unpublished outbox row, 0 if none.", nil, nil),
	}
}

func (c *outboxLagCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.oldestAgeSecond
}

func (c *outboxLagCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), outboxLagQueryTimeout)
	defer cancel()

	var count int64
	var ageSeconds float64
	err := c.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch from now() - min(created_at)), 0)
		FROM outbox
		WHERE published_at IS NULL
	`).Scan(&count, &ageSeconds)
	if err != nil {
		// A failed scrape of this one collector must not take the rest of
		// /metrics down with it -- report nothing for this round rather
		// than erroring the whole registry. Logged, though: this query
		// failing silently forever (e.g. the wrong DB role) would otherwise
		// never surface anywhere.
		if c.logger != nil {
			c.logger.Error("outbox lag query failed", "err", err)
		}
		return
	}
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(count))
	ch <- prometheus.MustNewConstMetric(c.oldestAgeSecond, prometheus.GaugeValue, ageSeconds)
}

// RegisterOutboxLag exposes how far the relay is behind: how many events
// are still unpublished, and how old the oldest of them is. Prometheus
// pulls, so a collector that queries at scrape time is enough -- no
// background ticker goroutine needed, unlike the relay's own polling loop.
// Must be called with a pool connected as wallet_relay: wallet_app (what
// cmd/wallet connects as) only has INSERT on outbox, never SELECT.
func RegisterOutboxLag(reg *prometheus.Registry, pool *Pool, logger *slog.Logger) {
	reg.MustRegister(newOutboxLagCollector(pool, logger))
}

// ============================================================================
// COMO ESSE ARQUIVO FUNCIONA (anotações de estudo)
// ============================================================================
//
// 1. RegisterPoolStats — métrica de graça
//
// pool (o *Pool deste pacote, pool.go) embute *pgxpool.Pool sem nome de
// campo, então pool.Stat() já está disponível hoje sem nenhum código novo no
// pgxpool em si — ele mesmo já conta, internamente, quantas conexões estão
// em uso, quantas estão ociosas, etc. Cada GaugeFunc aqui é só uma função
// que o Prometheus chama na hora do scrape pra ler esse contador — não
// existe nenhum estado guardado neste arquivo, o valor "atual" é sempre lido
// direto do pgxpool.
//
// 2. Por que o lag do outbox não é um GaugeFunc também
//
// A tentação óbvia seria repetir o padrão do item 1: um GaugeFunc pra
// "quantas linhas pendentes" e outro pra "idade da mais antiga". O problema
// é que isso rodaria a MESMA query duas vezes por scrape (uma pra cada
// GaugeFunc), dobrando a carga no banco pra nada — count(*) e min(created_at)
// já saem juntos de uma linha só. outboxLagCollector existe por isso: é um
// prometheus.Collector escrito à mão, cujo método Collect roda a query uma
// única vez e emite as duas métricas a partir do mesmo resultado.
//
// 3. Describe vs. Collect — o contrato de um Collector manual
//
// Describe diz ao Prometheus, de antemão (sem tocar o banco), quais métricas
// este Collector pode emitir — é assim que reg.MustRegister detecta conflito
// de nome antes de qualquer scrape acontecer. Collect é chamado a cada
// scrape de verdade: ele faz o trabalho (aqui, a query) e manda os valores
// pro canal. prometheus.NewDesc cria o "molde" (nome + help, sem labels
// nesse caso — nil, nil) que os dois métodos compartilham.
//
// 4. Por que Collect engole o erro da query em vez de propagar
//
// Um Collector não tem como devolver um error pro chamador (a assinatura de
// Collect não permite) — se a query falhar (banco fora, timeout), a opção é
// simplesmente não mandar nada pro canal nessa rodada. Isso significa que
// /metrics como um todo continua funcionando normalmente, só sem essas duas
// séries naquele scrape especificamente, em vez de a falha de uma query
// derrubar a resposta inteira do endpoint (que tem outras métricas, de
// outros coletores, que não têm nada a ver com isso).
