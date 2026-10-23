package postgres

import "time"

// resizeDecision is what a single tick of the resize policy concludes.
type resizeDecision int

const (
	resizeHold resizeDecision = iota
	resizeGrow
	resizeShrink
)

// resizePolicy is the pure, side-effect-free decision core for elastic
// pool resizing: given a saturation ratio and some tracked state, it
// decides whether to grow, shrink, or hold -- never touches pgx, never
// starts a goroutine. pool_resize.go is the thing that actually samples
// Stat(), calls decide(), and acts on the result.
type resizePolicy struct {
	highWatermark   float64
	lowWatermark    float64
	hysteresisTicks int
	cooldown        time.Duration
	step            int32
	minConns        int32
	maxConns        int32
}

// resizeState is the policy's memory across ticks. The caller owns it and
// passes it back in on every call -- decide never mutates anything behind
// your back, which is what makes it trivial to table-test.
type resizeState struct {
	highStreak   int
	lowStreak    int
	lastResizeAt time.Time
}

// decide looks at the current saturation ratio and returns what to do,
// plus the resizeState the caller must keep for the next tick.
func (p resizePolicy) decide(ratio float64, currentMaxConns int32, state resizeState, now time.Time) (resizeDecision, resizeState) {
	switch {
	case ratio >= p.highWatermark:
		state.highStreak++
		state.lowStreak = 0
	case ratio <= p.lowWatermark:
		state.lowStreak++
		state.highStreak = 0
	default:
		state.highStreak = 0
		state.lowStreak = 0
	}

	if !state.lastResizeAt.IsZero() && now.Sub(state.lastResizeAt) < p.cooldown {
		return resizeHold, state
	}

	if state.highStreak >= p.hysteresisTicks && currentMaxConns < p.maxConns {
		state.lastResizeAt = now
		state.highStreak = 0
		return resizeGrow, state
	}
	if state.lowStreak >= p.hysteresisTicks && currentMaxConns > p.minConns {
		state.lastResizeAt = now
		state.lowStreak = 0
		return resizeShrink, state
	}

	return resizeHold, state
}

// nextMaxConns applies decision's step to current, clamped to
// [minConns, maxConns]. Returns current unchanged for resizeHold.
func (p resizePolicy) nextMaxConns(current int32, decision resizeDecision) int32 {
	switch decision {
	case resizeGrow:
		return min(current+p.step, p.maxConns)
	case resizeShrink:
		return max(current-p.step, p.minConns)
	default:
		return current
	}
}

// ============================================================================
// COMO ESSE ARQUIVO FUNCIONA (anotações de estudo)
// ============================================================================
//
// 1. Por que separar a decisão do efeito collateral
//
// Redimensionar o pool de verdade (pool_resize.go) envolve pgx, goroutine,
// rede, Postgres real -- lento e só testável com Docker. A PERGUNTA "dado o
// que estou vendo agora, devo crescer, encolher ou não fazer nada?" não
// precisa de nada disso: é só aritmética sobre números. Separando essa
// pergunta numa função pura (resizePolicy.decide), ela vira testável em
// tabela, em milissegundos, sem container -- exatamente como o resto deste
// projeto trata regra de negócio vs. infraestrutura (mesma ideia do
// internal/domain nunca importar pgx, só que aplicada dentro de um único
// pacote de adapter em vez de entre camadas inteiras).
//
// 2. Histerese -- por que não decide na primeira amostra acima do watermark
//
// Uma rajada de tráfego é barulhenta: a taxa de ocupação do pool pode bater
// no teto por uma única leitura e cair no próximo tick sem que isso
// signifique "preciso de mais conexões para sempre". highStreak/lowStreak
// contam quantos ticks SEGUIDOS a taxa ficou acima (ou abaixo) do
// watermark -- só decide agir quando esse contador atinge hysteresisTicks.
// Qualquer leitura "no meio" (nem alta nem baixa) zera os dois contadores:
// não é "memória de longo prazo", é "está acontecendo agora, de forma
// sustentada".
//
// 3. Cooldown -- por que não é só histerese
//
// Histerese evita decidir cedo demais; cooldown evita decidir rápido
// demais depois de já ter decidido uma vez. Sem ele, o pool podia crescer,
// ver a taxa ainda alta três ticks depois (porque o pool novo ainda nem
// terminou de ficar pronto) e crescer de novo em sequência, sem dar tempo
// do resize anterior se refletir no sistema. `now.Sub(state.lastResizeAt) <
// p.cooldown` bloqueia qualquer decisão nova (mesmo que a histerese já
// tenha batido) até esse tempo passar.
//
// 4. nextMaxConns -- por que é uma função separada de decide
//
// decide só diz O QUE fazer (Grow/Shrink/Hold); nextMaxConns traduz isso em
// "qual o próximo MaxConns de verdade", já com o passo fixo (step) e o
// clamping nos limites (minConns/maxConns) aplicados. Separar os dois deixa
// cada um testável isoladamente: um teste de decide não precisa se importar
// com aritmética de clamping, e um teste de nextMaxConns não precisa
// simular histerese/cooldown.
