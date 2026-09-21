package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

func newTestAccount(t *testing.T, repo *AccountRepository, owner string, kind domain.AccountKind) uuid.UUID {
	t.Helper()
	a := Account{ID: uuid.New(), OwnerID: owner, Kind: kind, Currency: "BRL"}
	if err := repo.Create(context.Background(), a); err != nil {
		t.Fatalf("create account %q: %v", owner, err)
	}
	return a.ID
}

func mustTransfer(t *testing.T, exec *LockingTransferExecutor, from, to uuid.UUID, cents int64) {
	t.Helper()
	req := app.TransferRequest{ID: uuid.New(), FromAccountID: from, ToAccountID: to, Amount: domain.NewMoney(cents)}
	if err := exec.Execute(context.Background(), req); err != nil {
		t.Fatalf("transfer %d cents: %v", cents, err)
	}
}

func mustBalance(t *testing.T, entries *EntryRepository, id uuid.UUID) int64 {
	t.Helper()
	b, err := entries.SumByAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b.Cents()
}

func countTable(t *testing.T, pool *Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestLockingTransferExecutor_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	accounts, entries := NewAccountRepository(pool), NewEntryRepository(pool)
	exec := NewLockingTransferExecutor(pool)

	system := newTestAccount(t, accounts, "system", domain.AccountKindSystem)
	ana := newTestAccount(t, accounts, "ana", domain.AccountKindCustomer)
	bruno := newTestAccount(t, accounts, "bruno", domain.AccountKindCustomer)

	mustTransfer(t, exec, system, ana, 10000) // deposito
	mustTransfer(t, exec, ana, bruno, 3000)

	if got := mustBalance(t, entries, ana); got != 7000 {
		t.Errorf("ana = %d, want 7000", got)
	}
	if got := mustBalance(t, entries, bruno); got != 3000 {
		t.Errorf("bruno = %d, want 3000", got)
	}
	if got := mustBalance(t, entries, system); got != -10000 {
		t.Errorf("system = %d, want -10000", got)
	}
	assertInvariants(t, pool)
}

func TestLockingTransferExecutor_InsufficientFundsLeavesNothingBehind(t *testing.T) {
	pool := newTestPool(t)
	accounts, entries := NewAccountRepository(pool), NewEntryRepository(pool)
	exec := NewLockingTransferExecutor(pool)

	system := newTestAccount(t, accounts, "system", domain.AccountKindSystem)
	ana := newTestAccount(t, accounts, "ana", domain.AccountKindCustomer)
	bruno := newTestAccount(t, accounts, "bruno", domain.AccountKindCustomer)
	mustTransfer(t, exec, system, ana, 1000)

	transfersBefore, entriesBefore := countTable(t, pool, "transfers"), countTable(t, pool, "entries")

	req := app.TransferRequest{ID: uuid.New(), FromAccountID: ana, ToAccountID: bruno, Amount: domain.NewMoney(1001)}
	err := exec.Execute(context.Background(), req)
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}

	if got := countTable(t, pool, "transfers"); got != transfersBefore {
		t.Errorf("transfers = %d, want %d", got, transfersBefore)
	}
	if got := countTable(t, pool, "entries"); got != entriesBefore {
		t.Errorf("entries = %d, want %d", got, entriesBefore)
	}
	if got := mustBalance(t, entries, ana); got != 1000 {
		t.Errorf("ana = %d, want 1000", got)
	}
	assertInvariants(t, pool)
}

func TestLockingTransferExecutor_UnknownAccount(t *testing.T) {
	pool := newTestPool(t)
	accounts := NewAccountRepository(pool)
	exec := NewLockingTransferExecutor(pool)

	ana := newTestAccount(t, accounts, "ana", domain.AccountKindCustomer)

	req := app.TransferRequest{ID: uuid.New(), FromAccountID: ana, ToAccountID: uuid.New(), Amount: domain.NewMoney(100)}
	if err := exec.Execute(context.Background(), req); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
	if got := countTable(t, pool, "entries"); got != 0 {
		t.Errorf("entries = %d, want 0", got)
	}
}

// Opposite-direction transfers between the same two accounts are the
// classic deadlock. Locking in ascending id order must make them safe.
func TestLockingTransferExecutor_OppositeTransfersDoNotDeadlock(t *testing.T) {
	pool := newTestPool(t)
	accounts, entries := NewAccountRepository(pool), NewEntryRepository(pool)
	exec := NewLockingTransferExecutor(pool)

	system := newTestAccount(t, accounts, "system", domain.AccountKindSystem)
	a := newTestAccount(t, accounts, "a", domain.AccountKindCustomer)
	b := newTestAccount(t, accounts, "b", domain.AccountKindCustomer)
	mustTransfer(t, exec, system, a, 100000)
	mustTransfer(t, exec, system, b, 100000)

	const perDirection = 20
	var wg sync.WaitGroup
	for i := 0; i < perDirection; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := app.TransferRequest{ID: uuid.New(), FromAccountID: a, ToAccountID: b, Amount: domain.NewMoney(100)}
			if err := exec.Execute(context.Background(), req); err != nil {
				t.Errorf("a->b: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			req := app.TransferRequest{ID: uuid.New(), FromAccountID: b, ToAccountID: a, Amount: domain.NewMoney(100)}
			if err := exec.Execute(context.Background(), req); err != nil {
				t.Errorf("b->a: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := mustBalance(t, entries, a) + mustBalance(t, entries, b); got != 200000 {
		t.Errorf("a+b = %d, want 200000", got)
	}
	assertInvariants(t, pool)
}
