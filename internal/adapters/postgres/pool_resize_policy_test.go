package postgres

import (
	"testing"
	"time"
)

func testPolicy() resizePolicy {
	return resizePolicy{
		highWatermark:   0.85,
		lowWatermark:    0.30,
		hysteresisTicks: 3,
		cooldown:        45 * time.Second,
		step:            8,
		minConns:        5,
		maxConns:        30,
	}
}

func TestDecide_HoldsUntilHysteresisStreakCompletes(t *testing.T) {
	p := testPolicy()
	state := resizeState{}
	now := time.Now()

	// Two high ticks: not enough yet (hysteresisTicks == 3). currentMaxConns
	// stays below the ceiling (20 < maxConns==30) so there's room to grow
	// once the streak does complete.
	for i := 0; i < 2; i++ {
		var decision resizeDecision
		decision, state = p.decide(0.9, 20, state, now)
		if decision != resizeHold {
			t.Fatalf("tick %d: got %v, want resizeHold", i, decision)
		}
	}

	// Third consecutive high tick crosses the threshold.
	decision, _ := p.decide(0.9, 20, state, now)
	if decision != resizeGrow {
		t.Fatalf("3rd tick: got %v, want resizeGrow", decision)
	}
}

func TestDecide_MidRangeRatioResetsStreaks(t *testing.T) {
	p := testPolicy()
	state := resizeState{}
	now := time.Now()

	state.highStreak = 2                             // one tick away from growing
	decision, state := p.decide(0.5, 30, state, now) // neither high nor low
	if decision != resizeHold {
		t.Fatalf("got %v, want resizeHold", decision)
	}
	if state.highStreak != 0 {
		t.Fatalf("highStreak = %d, want reset to 0", state.highStreak)
	}
}

func TestDecide_ShrinkSymmetricToGrow(t *testing.T) {
	p := testPolicy()
	state := resizeState{}
	now := time.Now()

	for i := 0; i < 3; i++ {
		var decision resizeDecision
		decision, state = p.decide(0.1, 30, state, now)
		if i < 2 && decision != resizeHold {
			t.Fatalf("tick %d: got %v, want resizeHold", i, decision)
		}
		if i == 2 && decision != resizeShrink {
			t.Fatalf("3rd tick: got %v, want resizeShrink", decision)
		}
	}
}

func TestDecide_CooldownBlocksImmediateSecondResize(t *testing.T) {
	p := testPolicy()
	now := time.Now()
	state := resizeState{highStreak: 3, lastResizeAt: now.Add(-1 * time.Second)} // just resized

	decision, _ := p.decide(0.99, 30, state, now)
	if decision != resizeHold {
		t.Fatalf("got %v, want resizeHold during cooldown", decision)
	}
}

func TestDecide_CooldownExpiresAndAllowsResize(t *testing.T) {
	p := testPolicy()
	now := time.Now()
	state := resizeState{highStreak: 3, lastResizeAt: now.Add(-1 * time.Hour)} // long past cooldown

	decision, _ := p.decide(0.99, 20, state, now) // below ceiling, so there's room to grow
	if decision != resizeGrow {
		t.Fatalf("got %v, want resizeGrow once cooldown has elapsed", decision)
	}
}

func TestDecide_NeverGrowsPastMaxConns(t *testing.T) {
	p := testPolicy()
	state := resizeState{}
	now := time.Now()

	for i := 0; i < 3; i++ {
		_, state = p.decide(0.99, p.maxConns, state, now) // already at ceiling
	}
	decision, _ := p.decide(0.99, p.maxConns, state, now)
	if decision != resizeHold {
		t.Fatalf("got %v at ceiling, want resizeHold (nothing left to grow into)", decision)
	}
}

func TestDecide_NeverShrinksPastMinConns(t *testing.T) {
	p := testPolicy()
	state := resizeState{}
	now := time.Now()

	for i := 0; i < 3; i++ {
		_, state = p.decide(0.01, p.minConns, state, now) // already at floor
	}
	decision, _ := p.decide(0.01, p.minConns, state, now)
	if decision != resizeHold {
		t.Fatalf("got %v at floor, want resizeHold (nothing left to shrink into)", decision)
	}
}

func TestNextMaxConns(t *testing.T) {
	p := testPolicy()

	tests := []struct {
		name     string
		current  int32
		decision resizeDecision
		want     int32
	}{
		{"grow within bounds", 10, resizeGrow, 18},
		{"grow clamped at ceiling", 26, resizeGrow, 30},
		{"shrink within bounds", 20, resizeShrink, 12},
		{"shrink clamped at floor", 8, resizeShrink, 5},
		{"hold is a no-op", 15, resizeHold, 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.nextMaxConns(tt.current, tt.decision); got != tt.want {
				t.Errorf("nextMaxConns(%d, %v) = %d, want %d", tt.current, tt.decision, got, tt.want)
			}
		})
	}
}
