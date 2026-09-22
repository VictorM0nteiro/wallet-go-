package postgres

import (
	"context"
	"testing"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

func TestEntryRepository_ListByAccountPaginatesByCursor(t *testing.T) {
	pool := newTestPool(t)
	accounts, entries := NewAccountRepository(pool), NewEntryRepository(pool)
	exec := NewLockingTransferExecutor(pool)
	ctx := context.Background()

	system := newTestAccount(t, accounts, "system", domain.AccountKindSystem)
	ana := newTestAccount(t, accounts, "ana", domain.AccountKindCustomer)
	for _, cents := range []int64{100, 200, 300} {
		mustTransfer(t, exec, system, ana, cents)
	}

	first, err := entries.ListByAccount(ctx, ana, 0, 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 2 || first[0].AmountCents != 100 || first[1].AmountCents != 200 {
		t.Fatalf("first page = %+v, want amounts 100, 200", first)
	}

	second, err := entries.ListByAccount(ctx, ana, first[1].ID, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second) != 1 || second[0].AmountCents != 300 {
		t.Fatalf("second page = %+v, want amount 300", second)
	}
}
