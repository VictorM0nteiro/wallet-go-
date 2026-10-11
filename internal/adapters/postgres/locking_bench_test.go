package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// BenchmarkLockingTransfer_HotContention profiles the strategy the
// measurements in docs/bench/RESULTS.md (sections 6 and 7) show winning by
// a wide margin on both scenarios: every goroutine here transfers between
// the same two accounts, the same shape as the loadtest's "hot" scenario,
// so every attempt queues behind the same row lock. This bypasses
// idempotency (Execute, not the app.TransferService), to profile the
// executor itself, not the claim/replay bookkeeping around it.
//
// Capture CPU and heap profiles with (needs Docker, like every test in
// this package):
//
//	go test ./internal/adapters/postgres/ -run '^$' \
//	  -bench BenchmarkLockingTransfer_HotContention -benchtime 20s \
//	  -cpuprofile cpu.out -memprofile mem.out
//	go tool pprof -top cpu.out
//	go tool pprof -top mem.out
func BenchmarkLockingTransfer_HotContention(b *testing.B) {
	pool := newTestPool(b)
	accounts := NewAccountRepository(pool)
	exec := NewLockingTransferExecutor(pool)

	system := newTestAccount(b, accounts, "system", domain.AccountKindSystem)
	from := newTestAccount(b, accounts, "from", domain.AccountKindCustomer)
	to := newTestAccount(b, accounts, "to", domain.AccountKindCustomer)

	mustTransfer(b, exec, system, from, 1_000_000_000_00)

	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := app.TransferRequest{
				ID: uuid.New(), FromAccountID: from, ToAccountID: to, Amount: domain.NewMoney(1),
			}
			if err := exec.Execute(ctx, req); err != nil {
				b.Fatal(err)
			}
		}
	})
}
