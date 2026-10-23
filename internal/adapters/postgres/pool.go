package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig controls how the pgx connection pool behaves.
// AcquireTimeout bounds how long a caller waits to acquire a
// connection from the pool. Without it, a saturated pool or a slow
// database turns a request into one that hangs instead of one that
// fails fast — this is what Phase E's latency drill (roteiro 4 in
// docs/plano-execucao-wallet-go.md) exists to catch.
//
// MinConns turns on elastic resizing when > 0 (see pool_resize.go):
// the pool STARTS at MinConns (not MaxConns) and a background goroutine
// grows it toward MaxConns under sustained saturation, shrinking back
// down toward MinConns when idle. Left at zero (the default), the pool
// behaves exactly as before — created fixed at MaxConns, no extra
// goroutine, no room-to-grow needed because there's nothing growing.
type PoolConfig struct {
	DSN             string
	MinConns        int32
	MaxConns        int32
	MaxConnLifetime time.Duration
	AcquireTimeout  time.Duration
	// Logger receives resize decisions when MinConns > 0. Optional —
	// nil disables logging, not the resizing itself.
	Logger *slog.Logger
	// ResizeMetrics counts resize events when MinConns > 0. Optional,
	// nil-safe (see pool_resize.go / metrics.go).
	ResizeMetrics *poolResizeMetrics
}

const defaultAcquiretimeout = 5 * time.Second

// Pool wraps a live-swappable *pgxpool.Pool: every call below holds
// mu.RLock() for its full duration (not just to read the pointer), so a
// resize (pool_resize.go) can only swap in a new pgxpool.Pool once it
// holds mu.Lock() exclusively -- which is only possible once every
// in-flight call against the old pool has already finished. That's what
// makes it safe to close the old pool immediately after the swap (see
// pool_resize.go's resize()): nothing can still be mid-call against it.
//
// An earlier version used atomic.Pointer[pgxpool.Pool] instead -- reading
// the pointer was cheaper, but it only made the SWAP atomic, not "swap
// plus every in-flight call against the old pointer." A caller could
// Load() the old pool a moment before a resize, then actually call
// Exec/Query/etc. on it a moment after the resize's background goroutine
// had already closed it -- failing with "closed pool". A real
// integration test (pool_resize_test.go) caught this by running
// sustained concurrent load across an actual resize; RWMutex closes the
// gap because the resize cannot acquire mu.Lock() while any such call is
// still in progress.
type Pool struct {
	mu             sync.RWMutex
	pool           *pgxpool.Pool
	acquireTimeout time.Duration
	baseCfg        *pgxpool.Config
	resizer        *poolResizer // nil unless PoolConfig.MinConns > 0
}

// NewPool builds a ready-to-use connection pool from cfg and verifies
// connectivity with a Ping before returning.
func NewPool(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	pgxCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse pool config: %w", err)
	}

	ceiling := pgxCfg.MaxConns // pgx's own default, used only if cfg.MaxConns is unset
	if cfg.MaxConns > 0 {
		ceiling = cfg.MaxConns
	}
	pgxCfg.MaxConns = ceiling
	// An elastic pool starts small and grows under load (pool_resize.go) --
	// if it started at the ceiling, Grow would never have anywhere to go.
	if cfg.MinConns > 0 {
		pgxCfg.MaxConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pgxCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}

	acquireTimeout := cfg.AcquireTimeout
	if acquireTimeout <= 0 {
		acquireTimeout = defaultAcquiretimeout
	}

	rawPool, err := pgxpool.NewWithConfig(ctx, pgxCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()
	if err := rawPool.Ping(pingCtx); err != nil {
		rawPool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	p := &Pool{pool: rawPool, acquireTimeout: acquireTimeout, baseCfg: pgxCfg}

	if cfg.MinConns > 0 {
		logger := cfg.Logger
		if logger == nil {
			logger = slog.New(slog.DiscardHandler)
		}
		p.resizer = newPoolResizer(p, cfg.MinConns, ceiling, logger, cfg.ResizeMetrics)
		p.resizer.start()
	}

	return p, nil
}

// Exec, Query, QueryRow, Begin, BeginTx, SendBatch, Stat, Ping and Close
// below are explicit forwarding methods -- not struct embedding --
// precisely so a resize (pool_resize.go) can swap pool without any of
// these call sites noticing: AccountRepository, EntryRepository,
// LockingTransferExecutor, SerializableTransferExecutor, OutboxRelay and
// metrics.go all just call p.Exec(...)/p.Stat()/etc. on a *Pool exactly
// as before. Each one holds mu.RLock() for its whole duration -- see the
// Pool struct's doc comment for why that, not a cheaper atomic.Pointer
// read, is what actually makes the swap in pool_resize.go safe.

func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.Exec(ctx, sql, args...)
}

func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.Query(ctx, sql, args...)
}

func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.QueryRow(ctx, sql, args...)
}

func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.Begin(ctx)
}

func (p *Pool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.BeginTx(ctx, txOptions)
}

func (p *Pool) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.SendBatch(ctx, b)
}

func (p *Pool) Stat() *pgxpool.Stat {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.Stat()
}

func (p *Pool) Ping(ctx context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pool.Ping(ctx)
}

// Close stops any resize loop first, then closes the current pgxpool.Pool.
// Stopping the resizer before closing is what prevents a resize from
// racing against shutdown -- see pool_resize.go's stop() for how that
// join actually happens.
func (p *Pool) Close() {
	if p.resizer != nil {
		p.resizer.stop()
	}
	p.mu.RLock()
	pool := p.pool
	p.mu.RUnlock()
	pool.Close()
}

// withAcquireTimeout returns a copy of ctx bounded by the pool's configured
// AcquireTimeout. Repositories call this before every query so a slow or
// saturated database surfaces as a fast, typed timeout instead of a hang.
func (p *Pool) withAcquireTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.acquireTimeout)
}

// ============================================================================
// COMO ESSE ARQUIVO FUNCIONA (anotações de estudo)
// ============================================================================
//
// 1. O problema que isso resolve
//
// Toda vez que o programa precisa falar com o Postgres, alguém tem que abrir
// uma conexão TCP, fazer o handshake, autenticar... isso é caro (milissegundos).
// Se abríssemos uma conexão nova a cada query, a API ficaria lenta e o
// Postgres ficaria sobrecarregado com handshakes.
//
// A solução padrão é um connection pool: abrimos um punhado de conexões
// (ex: 10) logo na inicialização, e elas ficam vivas, reutilizadas por todas
// as requisições. Quando uma goroutine precisa falar com o banco, ela "pega
// emprestada" uma conexão do pool, usa, devolve. Se as 10 estiverem
// ocupadas, a próxima requisição espera na fila. pgxpool (do driver pgx,
// ver docs/escopo-carteira-go.md §9) implementa esse pool pra gente.
//
// 2. PoolConfig — os parâmetros do pool
//
//	DSN: a string de conexão (ex: postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable),
//	  igual à DATABASE_URL do Makefile.
//	MaxConns: quantas conexões o pool pode ter abertas ao mesmo tempo, no
//	  máximo — o tamanho da "piscina".
//	MaxConnLifetime: depois de quanto tempo uma conexão individual é
//	  descartada e recriada, mesmo saudável. Rede de segurança contra
//	  conexões antigas que acumulam problema silencioso (proxy de rede
//	  no meio, por exemplo).
//	AcquireTimeout: quanto tempo uma goroutine espera na fila por uma
//	  conexão livre antes de desistir e retornar erro. Esse é o parâmetro
//	  que o plano de execução chamou de armadilha: sem ele, um pool cheio
//	  (ou banco lento) deixa a requisição pendurada pra sempre em vez de
//	  falhar rápido — sintoma que a Fase E (roteiro 4, latência artificial)
//	  existe pra pegar.
//
// 3. defaultAcquiretimeout
//
// Valor padrão (5s) usado se quem chama NewPool deixar AcquireTimeout
// zerado — rede de segurança pra nunca rodar sem timeout por esquecimento.
//
// 4. O struct Pool — por que RWMutex em vez de embutir o *pgxpool.Pool
//
// A versão original deste arquivo (antes do pool elástico, ADR 9) embutia
// *pgxpool.Pool sem nome de campo (struct embedding): todos os métodos
// dele (.Exec(), .Query(), .Close()) ficavam disponíveis automaticamente,
// como se fossem herdados. Isso parou de funcionar quando o pool passou a
// poder ser TROCADO em runtime (pool_resize.go) — embedding é uma
// referência fixa, não dá pra trocar "o que está embutido" depois que o
// struct já existe.
//
// A primeira tentativa de corrigir isso foi um atomic.Pointer[pgxpool.Pool]:
// cada método fazia current.Load() antes de delegar. Rápido, mas
// incompleto — um teste de integração de verdade (pool_resize_test.go,
// carga sustentada atravessando um resize real) pegou uma corrida: uma
// goroutine podia dar Load() no pool antigo um instante antes da troca, e
// só chamar .Exec() nele um instante DEPOIS do pool antigo já ter sido
// fechado em segundo plano — erro "closed pool" sem nenhum motivo de
// negócio. atomic.Pointer deixava a TROCA atômica, mas não "troca mais
// toda chamada em andamento contra o ponteiro antigo".
//
// A versão final usa sync.RWMutex: cada método (Exec, Query, Stat, ...)
// segura mu.RLock() durante TODA a chamada, não só na hora de ler o
// ponteiro. Um resize só consegue mu.Lock() (exclusivo) quando nenhuma
// chamada está em andamento — e, por construção do Go, isso significa que
// no instante em que o resize termina de trocar p.pool e libera o lock,
// nenhuma goroutine pode mais estar segurando uma referência ao pool antigo.
// Esse é o ganho real: provar "ninguém está no meio de uma chamada"
// através do próprio mutex, em vez de confiar em um Close() que só sabe
// esperar conexões JÁ adquiridas, não chamadas que ainda nem começaram.
//
// 5. NewPool — passo a passo
//
//	pgxCfg, err := pgxpool.ParseConfig(cfg.DSN)
//	  transforma a DSN numa struct de configuração que o pgxpool entende.
//
//	ceiling := ...; if cfg.MinConns > 0 { pgxCfg.MaxConns = cfg.MinConns }
//	  um pool elástico começa PEQUENO (no piso) e cresce sob carga — se
//	  começasse já no teto, Grow nunca teria pra onde crescer. Sem
//	  elasticidade (MinConns == 0), o comportamento é o de sempre: cria
//	  fixo no teto.
//
//	rawPool, err := pgxpool.NewWithConfig(ctx, pgxCfg)
//	  cria o pool de verdade (ainda não testou se fala com o banco).
//
//	pingCtx, cancel := context.WithTimeout(ctx, acquireTimeout); defer cancel()
//	rawPool.Ping(pingCtx)
//	  "teste de fumaça": manda um PING pro Postgres pra confirmar que a
//	  conexão funciona antes de devolver o pool pra quem chamou. Se falhar,
//	  fecha tudo (rawPool.Close(), pra não vazar conexão) e retorna erro
//	  claro, em vez de só descobrir na primeira query real já em produção.
//	  O mesmo Ping se repete a cada resize (pool_resize.go), não só aqui.
//
// 6. withAcquireTimeout — o "carimbo de prazo" reutilizável
//
// Método minúsculo (só usável dentro do pacote postgres) que os
// repositórios chamam antes de cada query, em vez de cada um lembrar de
// criar seu próprio timeout. É assim que AcquireTimeout de fato "pega" em
// toda operação do banco — sem isso, ficaria só documentado, sem efeito
// nenhum no código. Uso típico dentro de um repositório:
//
//	ctx, cancel := r.pool.withAcquireTimeout(ctx)
//	defer cancel()
//	// ... faz a query usando esse ctx
