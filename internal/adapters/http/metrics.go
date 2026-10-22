package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry builds a fresh Prometheus registry -- never
// prometheus.DefaultRegisterer. Same rule newPerScopeLimiters already
// follows for rate limiting: every NewRouter call (the real server, and
// every test) must not share metrics state with another.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// httpMetricsRecorder holds the descriptors one NewRouter call registers
// into its own registry.
type httpMetricsRecorder struct {
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	errors    *prometheus.CounterVec
	transfers *prometheus.CounterVec
}

func newHTTPMetrics(reg *prometheus.Registry) *httpMetricsRecorder {
	m := &httpMetricsRecorder{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_http_requests_total",
			Help: "HTTP requests, by method, matched route pattern, and status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wallet_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by method and matched route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_business_errors_total",
			Help: "Domain/application errors, by endpoint and the statusFor() error code.",
		}, []string{"endpoint", "code"}),
		transfers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_transfers_total",
			Help: "Money-moving requests completed, by endpoint and whether the response was a replay.",
		}, []string{"endpoint", "replayed"}),
	}
	reg.MustRegister(m.requests, m.duration, m.errors, m.transfers)
	return m
}

// httpMetrics records request count and duration for every request,
// including unauthenticated and rejected ones (401, 403, 429) -- it sits in
// the top-level middleware chain, outside apiKeyAuth, the same place
// accessLog sits, so /healthz, /readyz and auth/rate-limit rejections all
// show up in the same series as everything else.
func httpMetrics(m *httpMetricsRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r)

			// The matched route pattern (e.g. "/accounts/{id}/balance"), never
			// the raw path -- one series per account UUID would be a
			// cardinality explosion, not a dashboard.
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			m.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
			m.duration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
		})
	}
}

func metricsHandler(reg *prometheus.Registry) http.HandlerFunc {
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return h.ServeHTTP
}

// ============================================================================
// COMO ESSE ARQUIVO FUNCIONA (anotações de estudo)
// ============================================================================
//
// 1. O que isso adiciona ao projeto
//
// GET /metrics expõe, no formato de texto que o Prometheus sabe ler
// (promhttp.HandlerFor), quatro famílias de métrica: quantas requisições
// chegaram e com que status (wallet_http_requests_total), quanto tempo cada
// uma levou (wallet_http_request_duration_seconds), quantos erros de negócio
// aconteceram e com qual código (wallet_business_errors_total), e quantas
// transferências/depósitos/saques terminaram, distinguindo replay de
// idempotência de execução nova (wallet_transfers_total). ADR 8 documenta a
// escolha de usar o client direto do Prometheus em vez do SDK completo do
// OpenTelemetry.
//
// 2. Por que NewRegistry() nunca usa prometheus.DefaultRegisterer
//
// DefaultRegisterer é uma variável global do pacote prometheus: qualquer
// código que chamasse prometheus.MustRegister(...) sem passar um registry
// explícito estaria escrevendo nessa mesma variável compartilhada. Isso
// quebraria exatamente a mesma garantia que newPerScopeLimiters já protege
// pra rate limiting: cada chamada de NewRouter (o servidor real, e cada
// teste) precisa das suas próprias métricas, zeradas, sem vazar contagem de
// um teste pro outro. Por isso NewRegistry cria um *prometheus.Registry novo
// a cada chamada, e tudo (GoCollector, ProcessCollector, os quatro
// descritores de newHTTPMetrics) é registrado nele, nunca no global.
//
// 3. httpMetricsRecorder — por que agrupar os quatro descritores num struct
//
// CounterVec e HistogramVec são os "moldes" com os labels declarados
// (method/route/status, etc.) — esses moldes não guardam valor nenhum até
// alguém chamar .WithLabelValues(...).Inc() ou .Observe(...) numa combinação
// específica de labels, que aí sim cria (ou reaproveita) uma série dentro do
// Vec. Handler guarda um *httpMetricsRecorder (campo "metrics") pra poder
// incrementar wallet_business_errors_total e wallet_transfers_total direto
// de dentro de writeError (respond.go) e dos handlers que chamam
// writeResult (handler.go) — nenhum dos dois precisa saber como o registry
// foi montado, só precisa do recorder.
//
// 4. httpMetrics — por que ele reusa statusRecorder em vez de criar outro
//
// statusRecorder já existe em middleware.go e é usado por accessLog
// exatamente pra isso: embrulhar o http.ResponseWriter real, deixar a
// chamada seguinte (next.ServeHTTP) escrever normalmente, e guardar qual
// status code foi escrito, sem mudar o comportamento de nada. httpMetrics
// cria o seu próprio statusRecorder, separado do de accessLog (que já
// rodou por fora, nessa altura da cadeia) — dois wrappers por requisição é
// redundante, mas é o mesmo estilo que recoverer já usa (cada middleware
// reage sozinho, sem depender dos vizinhos saberem um do outro).
//
// 5. route := chi.RouteContext(r.Context()).RoutePattern() — a parte que
// evita destruir o Prometheus
//
// Prometheus guarda uma série de dados por combinação única de labels. Se o
// label "route" fosse o path crú da requisição (r.URL.Path), toda conta
// diferente ("/accounts/<uuid-1>/balance", "/accounts/<uuid-2>/balance", ...)
// criaria uma série nova, pra sempre — isso é "explosão de cardinalidade":
// o Prometheus vai ficando mais lento e consumindo mais memória a cada conta
// nova criada, sem limite. RoutePattern() devolve o padrão que o chi casou
// ("/accounts/{id}/balance"), que é sempre o mesmo não importa qual UUID foi
// usado — por isso o número de séries fica limitado ao número de rotas que
// o router de fato tem, não ao número de contas que existem no banco. O
// fallback "unmatched" cobre o caso de uma rota que não bateu com nada
// (404): sem ele, RoutePattern() voltaria vazio, e cada path inventado por
// um scanner batendo na API geraria sua própria série também.
//
// 6. metricsHandler — por que devolver http.HandlerFunc, não http.Handler
//
// promhttp.HandlerFor devolve um http.Handler (uma interface). O roteador
// chi (r.Get, no router.go) espera um http.HandlerFunc especificamente, não
// qualquer coisa que satisfaça a interface http.Handler — então
// metricsHandler pega o método ServeHTTP do handler que o promhttp construiu
// e devolve só ele, que já tem exatamente o formato que http.HandlerFunc
// exige (func(http.ResponseWriter, *http.Request)).
