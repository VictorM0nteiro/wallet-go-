package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

func TestAccountAndEntryRepositories_DepositEndToEnd(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accounts := NewAccountRepository(pool)
	entries := NewEntryRepository(pool)

	system := app.Account{ID: uuid.New(), OwnerID: "system", Kind: domain.AccountKindSystem, Currency: "BRL"}
	customer := app.Account{ID: uuid.New(), OwnerID: "ana", Kind: domain.AccountKindCustomer, Currency: "BRL"}

	if err := accounts.Create(ctx, system); err != nil {
		t.Fatalf("create system account: %v", err)
	}
	if err := accounts.Create(ctx, customer); err != nil {
		t.Fatalf("create customer account: %v", err)
	}

	got, err := accounts.FindByID(ctx, customer.ID)
	if err != nil {
		t.Fatalf("find customer account: %v", err)
	}
	if got.OwnerID != customer.OwnerID {
		t.Fatalf("OwnerID = %q, want %q", got.OwnerID, customer.OwnerID)
	}

	// A transferência ainda não tem repositório próprio (isso é Sessão 3).
	// A linha em `transfers` é criada direto aqui só pra satisfazer a FK
	// que `entries.transfer_id` exige.
	transferID := uuid.New()
	const insertTransfer = `
              INSERT INTO transfers (id, from_account_id, to_account_id, amount_cents, status)
              VALUES ($1, $2, $3, $4, 'completed')
      `
	if _, err := pool.Exec(ctx, insertTransfer, transferID, system.ID, customer.ID, int64(5000)); err != nil {
		t.Fatalf("seed transfer: %v", err)
	}

	pair := []Entry{
		{TransferID: transferID, AccountID: system.ID, AmountCents: -5000},
		{TransferID: transferID, AccountID: customer.ID, AmountCents: 5000},
	}
	if err := entries.InsertBatch(ctx, pair); err != nil {
		t.Fatalf("insert entries: %v", err)
	}

	balance, err := entries.SumByAccount(ctx, customer.ID)
	if err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if balance.Cents() != 5000 {
		t.Fatalf("balance = %d, want 5000", balance.Cents())
	}
}

func TestAccountRepository_FindByID_NotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accounts := NewAccountRepository(pool)

	_, err := accounts.FindByID(ctx, uuid.New())
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}
