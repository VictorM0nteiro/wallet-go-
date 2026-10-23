package postgres

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestElasticPool_GrowsUnderSustainedLoadWithoutBreakingInFlightQueries
// drives real, sustained saturation against a real Postgres container and
// waits for the real resize policy (actual constants: 3s ticks, 3-tick
// hysteresis, see pool_resize.go) to actually grow MaxConns -- not a mock
// of the policy, the genuine end-to-end mechanism. It also keeps a
// concurrent stream of queries running across the resize boundary: if the
// atomic swap in pool_resize.go ever broke an in-flight query, one of
// these goroutines would report an error.
func TestElasticPool_GrowsUnderSustainedLoadWithoutBreakingInFlightQueries(t *testing.T) {
	_, dsn := newTestPoolWithDSN(t)

	reg := prometheus.NewRegistry()
	metrics := NewPoolResizeMetrics(reg)
	logger := slog.New(slog.NewTextHandler(newTestLogWriter(t), nil))

	const floor = int32(3)    // where the pool starts (PoolConfig.MinConns)
	const ceiling = int32(10) // what the resize policy may grow it toward

	ctx := context.Background()
	pool, err := NewPool(ctx, PoolConfig{
		DSN:             dsn,
		MinConns:        floor,
		MaxConns:        ceiling,
		MaxConnLifetime: time.Minute,
		AcquireTimeout:  5 * time.Second,
		Logger:          logger,
		ResizeMetrics:   metrics,
	})
	if err != nil {
		t.Fatalf("create elastic pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if got := pool.Stat().MaxConns(); got != floor {
		t.Fatalf("elastic pool must start at the floor (%d), got %d", floor, got)
	}

	loadCtx, cancelLoad := context.WithTimeout(ctx, 20*time.Second)
	defer cancelLoad()

	var wg sync.WaitGroup
	errCh := make(chan error, int(floor)*2)
	holder := func() {
		defer wg.Done()
		for {
			select {
			case <-loadCtx.Done():
				return
			default:
			}
			if _, err := pool.Exec(loadCtx, "SELECT pg_sleep(0.05)"); err != nil {
				if loadCtx.Err() != nil {
					return // context expired mid-query, not a real failure
				}
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}
	for range floor * 2 {
		wg.Add(1)
		go holder()
	}

	grew := waitForCondition(t, 18*time.Second, func() bool {
		return pool.Stat().MaxConns() > floor
	})

	cancelLoad()
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("query failed during/around a resize: %v", err)
	}

	if !grew {
		t.Fatalf("Stat().MaxConns() never grew past the floor (%d) within the timeout (last seen: %d)",
			floor, pool.Stat().MaxConns())
	}
}

// waitForCondition polls cond every 200ms until it returns true or timeout
// elapses. Used instead of a fixed sleep because the resize policy's exact
// timing (tick interval x hysteresis ticks) is an implementation detail
// this test shouldn't hardcode a sleep duration around.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return cond()
}

// newTestLogWriter routes the resizer's slog output through t.Log, so a
// failing test shows what the resize loop actually decided instead of
// nothing (os.Stdout output from a goroutine is easy to miss in test
// output otherwise).
type testLogWriter struct{ t testing.TB }

func newTestLogWriter(t testing.TB) testLogWriter { return testLogWriter{t: t} }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
