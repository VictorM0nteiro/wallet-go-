package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// Account is the row shape of the accounts table. It is a persistence
// concern, not a domain entity — internal/domain has no Account type yet
// (a deliberate Bloco 4 decision), so this struct only borrows
// domain.AccountKind for the Kind field.
type Account struct {
	ID        uuid.UUID
	OwnerID   string
	Kind      domain.AccountKind
	Currency  string
	CreatedAt time.Time
}

// AccountRepository reads and writes the accounts table.
type AccountRepository struct {
	pool *Pool
}

// NewAccountRepository builds an AccountRepository backed by pool.
func NewAccountRepository(pool *Pool) *AccountRepository {
	return &AccountRepository{pool: pool}
}

// Create inserts a new account row.
func (r *AccountRepository) Create(ctx context.Context, a Account) error {
	ctx, cancel := r.pool.withAcquireTimeout(ctx)
	defer cancel()

	const query = `
              INSERT INTO accounts (id, owner_id, kind, currency)
              VALUES ($1, $2, $3, $4)
      `
	_, err := r.pool.Exec(ctx, query, a.ID, a.OwnerID, string(a.Kind), a.Currency)
	if err != nil {
		return fmt.Errorf("postgres: create account: %w", err)
	}
	return nil
}

// FindByID looks up an account by id. It returns ErrAccountNotFound if no
// row matches.
func (r *AccountRepository) FindByID(ctx context.Context, id uuid.UUID) (Account, error) {
	ctx, cancel := r.pool.withAcquireTimeout(ctx)
	defer cancel()

	const query = `
              SELECT id, owner_id, kind, currency, created_at
              FROM accounts
              WHERE id = $1
      `

	var (
		a    Account
		kind string
	)
	err := r.pool.QueryRow(ctx, query, id).Scan(&a.ID, &a.OwnerID, &kind, &a.Currency, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrAccountNotFound
		}
		return Account{}, fmt.Errorf("postgres: find account: %w", err)
	}
	a.Kind = domain.AccountKind(kind)

	return a, nil
}
