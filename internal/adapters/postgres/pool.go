package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig controls how the pgx connection pool behaves.
// AcquireTimeout bounds how long a caller waits to acquire a
// connection from the pool. Without it, a saturated pool or a slow
// database turns a request into one that hangs instead of one that
// fails fast — this is what Phase E's latency drill (roteiro 4 in
// docs/plano-execucao-wallet-go.md) exists to catch.
type PoolConfig struct {
	DSN             string
	MaxConns        int32
	MaxConnLifetime time.Duration
	AcquireTimeout  time.Duration
}

const defaultAcquiretimeout = 5 * time.Second

// Pool wraps *pgxpool.Pool to enforce AcquireTimeout on every operation
// that goes through it, instead of leaving each caller to remember.
type Pool struct {
	*pgxpool.Pool
	acquireTimeout time.Duration
}

// NewPool builds a ready-to-use connection pool from cfg and verifies
// connectivity with a Ping before returning.
func NewPool(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	pgxCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse pool config: %w", err)
	}

	if cfg.MaxConns > 0 {
		pgxCfg.MaxConns = cfg.MaxConns
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

	return &Pool{Pool: rawPool, acquireTimeout: acquireTimeout}, nil
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
// 4. O struct Pool — por que "embutir" o *pgxpool.Pool?
//
// Isso é struct embedding: colocar *pgxpool.Pool sem nome de campo dentro
// de Pool faz todos os métodos dele (.Exec(), .Query(), .Close()) ficarem
// disponíveis automaticamente no nosso Pool, como se fossem herdados —
// meuPool.Exec(...) funciona sem escrever um método Exec que só repassa
// pra dentro. A casca existe só pra guardar acquireTimeout junto, pra os
// repositórios (próximo arquivo) usarem sem cada um saber esse valor
// separadamente.
//
// 5. NewPool — passo a passo
//
//	pgxCfg, err := pgxpool.ParseConfig(cfg.DSN)
//	  transforma a DSN numa struct de configuração que o pgxpool entende.
//
//	if cfg.MaxConns > 0 { pgxCfg.MaxConns = ... }
//	  só sobrescreve o default do pgx se um valor explícito foi passado
//	  (mesma lógica pra MaxConnLifetime).
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
//	  context.WithTimeout cria um "relógio": depois do prazo, o contexto
//	  expira sozinho e o Ping é obrigado a desistir. defer cancel() libera
//	  os recursos desse relógio assim que a função termina — prática
//	  padrão sempre que se cria um context.WithTimeout/WithCancel.
//
//	return &Pool{Pool: rawPool, acquireTimeout: acquireTimeout}, nil
//	  devolve o wrapper: o pool de verdade + o timeout guardado.
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
