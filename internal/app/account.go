package app

import (
	"context"
	"errors"
	"strings"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
	"github.com/google/uuid"
)

// ErrInvalidOwner is returned when an account is created without an owner.
var ErrInvalidOwner = errors.New("app: owner_id must not be empty")

const defaultCurrency = "BRL"

// NewAccountService builds an AccountService.
func NewAccountService(accounts AccountStore, ledger LedgerReader) *AccountService {
	return &AccountService{accounts: accounts, ledger: ledger}
}

// Create registers a new customer account.
func (s *AccountService) Create(ctx context.Context, ownerID string) (Account, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return Account{}, ErrInvalidOwner
	}
	a := Account{
		ID:       uuid.New(),
		OwnerID:  ownerID,
		Kind:     domain.AccountKindCustomer,
		Currency: defaultCurrency,
	}
	if err := s.accounts.Create(ctx, a); err != nil {
		return Account{}, err
	}
	return s.accounts.FindByID(ctx, a.ID)
}

// Balance derives the account balance from the ledger.
func (s *AccountService) Balance(ctx context.Context, accountID uuid.UUID) (domain.Money, error) {
	if _, err := s.accounts.FindByID(ctx, accountID); err != nil {
		return 0, err
	}
	return s.ledger.SumByAccount(ctx, accountID)
}

// Entries returns one page of the statement, oldest first. limit must be
// positive; it fetches limit+1 rows to know whether another page exists.
func (s *AccountService) Entries(ctx context.Context, accountID uuid.UUID, afterID int64, limit int) (EntriesPage, error) {
	if _, err := s.accounts.FindByID(ctx, accountID); err != nil {
		return EntriesPage{}, err
	}
	rows, err := s.ledger.ListByAccount(ctx, accountID, afterID, limit+1)
	if err != nil {
		return EntriesPage{}, err
	}
	page := EntriesPage{Entries: rows}
	if len(rows) > limit {
		page.Entries = rows[:limit]
		next := rows[limit-1].ID
		page.NextAfter = &next
	}
	return page, nil
}
