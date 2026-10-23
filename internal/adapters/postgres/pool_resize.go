package postgres

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Resize policy constants. These are deliberately not environment
// variables -- see docs/adr/0009-elastic-db-pool.md: only the floor
// (WALLET_DB_MIN_CONNS) and the existing ceiling (WALLET_DB_MAX_CONNS)
// are operator-tunable, everything about *how* the policy reacts is a
// named constant close to the code that uses it.
const (
	resizeCheckInterval   = 3 * time.Second
	resizeHighWatermark   = 0.85
	resizeLowWatermark    = 0.30
	resizeHysteresisTicks = 3
	resizeCooldown        = 45 * time.Second
	resizeStep            = int32(8)
)

// poolResizer owns the background goroutine that samples Stat(), asks
// resizePolicy what to do, and -- if it says Grow or Shrink -- builds a
// replacement pgxpool.Pool and swaps it into the owning Pool's current
// pointer. See pool.go's Pool.current / Pool.Close for how it's wired in.
type poolResizer struct {
	pool    *Pool
	policy  resizePolicy
	logger  *slog.Logger
	metrics *poolResizeMetrics

	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup
}

func newPoolResizer(pool *Pool, minConns, maxConns int32, logger *slog.Logger, metrics *poolResizeMetrics) *poolResizer {
	return &poolResizer{
		pool: pool,
		policy: resizePolicy{
			highWatermark:   resizeHighWatermark,
			lowWatermark:    resizeLowWatermark,
			hysteresisTicks: resizeHysteresisTicks,
			cooldown:        resizeCooldown,
			step:            resizeStep,
			minConns:        minConns,
			maxConns:        maxConns,
		},
		logger:  logger,
		metrics: metrics,
		done:    make(chan struct{}),
	}
}

func (r *poolResizer) start() {
	r.wg.Add(1)
	go r.loop()
}

// stop signals the loop to exit and blocks until it actually has --
// called from Pool.Close() before the current pgxpool.Pool is closed, so
// no resize can be mid-flight (or start fresh) during shutdown.
func (r *poolResizer) stop() {
	r.stopOnce.Do(func() { close(r.done) })
	r.wg.Wait()
}

func (r *poolResizer) loop() {
	defer r.wg.Done()

	ticker := time.NewTicker(resizeCheckInterval)
	defer ticker.Stop()

	var state resizeState
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			state = r.tick(state)
		}
	}
}

func (r *poolResizer) tick(state resizeState) resizeState {
	stat := r.pool.Stat()
	maxConns := stat.MaxConns()
	if maxConns <= 0 {
		return state
	}
	ratio := float64(stat.AcquiredConns()) / float64(maxConns)

	decision, newState := r.policy.decide(ratio, maxConns, state, time.Now())
	if decision == resizeHold {
		return newState
	}

	target := r.policy.nextMaxConns(maxConns, decision)
	if target == maxConns {
		return newState
	}

	r.resize(target, decision)
	return newState
}

// resize builds a brand-new pgxpool.Pool at the target size, verifies it
// with a Ping before committing to it, swaps it in under an exclusive
// lock, and closes the old pool in a detached goroutine (pgxpool.Pool.
// Close blocks until every connection it ever created -- including ones
// currently checked out -- is returned and destroyed, so this drains
// gracefully on its own; it must not run on this goroutine or a single
// slow query would stall every future resize decision). The swap takes
// r.pool.mu.Lock() -- not just a pointer store -- specifically so it
// cannot complete while any forwarding method (pool.go) is still
// mid-call against the old pool; see Pool's doc comment for why that
// distinction is what actually makes closing the old pool immediately
// afterward safe.
func (r *poolResizer) resize(target int32, decision resizeDecision) {
	cfg := r.pool.baseCfg.Copy()
	cfg.MaxConns = target

	ctx, cancel := context.WithTimeout(context.Background(), r.pool.acquireTimeout)
	defer cancel()

	newPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		r.logger.Error("pool resize: create new pool failed", "target_max_conns", target, "err", err)
		return
	}
	if err := newPool.Ping(ctx); err != nil {
		r.logger.Error("pool resize: ping new pool failed, aborting resize", "target_max_conns", target, "err", err)
		newPool.Close()
		return
	}

	r.pool.mu.Lock()
	old := r.pool.pool
	r.pool.pool = newPool
	r.pool.mu.Unlock()

	direction := "grow"
	if decision == resizeShrink {
		direction = "shrink"
	}
	r.logger.Info("pool resize: applied", "direction", direction, "new_max_conns", target)
	r.metrics.incResize(direction)

	go old.Close()
}

// ============================================================================
// COMO ESSE ARQUIVO FUNCIONA (anotações de estudo)
// ============================================================================
//
// 1. Por que existe, resumido
//
// pgxpool.Pool não tem API pra mudar MaxConns depois de criado (conferido
// lendo o código-fonte do pgx v5.11.0 em si, não assumido). A única forma
// de ter elasticidade de verdade é ter DOIS pgxpool.Pool em algum memento
// (o velho e o novo) e trocar qual deles está "ativo" -- pool.go guarda
// esse "ativo" num atomic.Pointer, e este arquivo é quem decide quando
// trocar e constrói o substitution.
//
// 2. loop/tick — por que tick é uma função separada do loop
//
// loop só cuida do relógio (ticker, canal done pra parar). tick faz o
// trabalho de verdade de uma rodada: lê Stat(), pergunta pra resizePolicy
// o que fazer, e se for Grow/Shrink, chama resize(). Separar assim deixa
// tick testável sem precisar de um ticker de verdade rodando (não que haja
// teste direto de tick aqui -- a cobertura de integração roda o loop
// inteiro via start()/stop(), mas a separação ainda documenta a
// responsabilidade de cada pedaço).
//
// 3. resize — por que o Ping acontece ANTES da troca, nunca depois
//
// Se o Postgres estiver, por exemplo, no próprio limite de max_connections
// do servidor, criar um pgxpool.Pool novo pode "funcionar" (não erra na
// hora de criar a struct) mas falhar na primeira conexão de verdade. O
// Ping aqui é exatamente o mesmo teste de fumaça que NewPool já faz na
// inicialização (pool.go) -- só que agora repetido a cada resize. Se falhar,
// a função simplesmente desiste (fecha o pool recém-criado, não tentado) e
// o pool antigo continua servindo normalmente -- um resize que não deu
// certo nunca deve deixar o sistema pior do que estava.
//
// 4. old.Close() numa goroutine separada — por que não "só fechar e pronto"
//
// pgxpool.Pool.Close() bloqueia até toda conexão que ele já criou --
// inclusive as que alguma query ainda está segurando naquele exato instante
// -- ser devolvida e destruída (comportamento confirmado lendo o código do
// puddle, a biblioteca de pooling por trás do pgxpool). Isso é ótimo pra
// drenagem graciosa, mas péssimo se rodar na mesma goroutine do loop de
// resize: uma única query lenta no pool antigo travaria essa goroutine, e
// com ela, toda decisão de resize futura (o ticker continua disparando,
// mas tick() não roda de novo até resize() retornar). `go old.Close()`
// desacopla: o pool antigo drena no próprio ritmo, em segundo plano,
// enquanto o loop principal já está livre pra reagir à próxima rodada.
//
// 5. Por que os parâmetros da política são constantes, não env vars
//
// WALLET_DB_MIN_CONNS (cmd/wallet/main.go) e WALLET_DB_MAX_CONNS (já
// existente) são os únicos dois números realmente operacionais -- o piso e
// o teto que um operador de verdade pode querer mudar por ambiente. Os
// outros (watermarks, histerese, cooldown, passo) são decisões de
// comportamento do mecanismo em si, não de ambiente -- documentados e
// justificados na ADR 9, mas mantidos como constante pra não criar uma
// superfície de configuração que, na prática, ninguém ajusta.
