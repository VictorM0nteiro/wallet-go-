package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// pgUniqueViolation is the SQLSTATE for unique_violation.
const pgUniqueViolation = "23505"

// AccountRepository reads and writes the accounts table. It implements
// app.AccountStore.
type AccountRepository struct {
	pool *Pool
}

// NewAccountRepository builds an AccountRepository backed by pool.
func NewAccountRepository(pool *Pool) *AccountRepository {
	return &AccountRepository{pool: pool}
}

// Create inserts a new account row. It returns
// domain.ErrAccountAlreadyExists if (owner, kind, currency) is taken.
func (r *AccountRepository) Create(ctx context.Context, a app.Account) error {
	ctx, cancel := r.pool.withAcquireTimeout(ctx)
	defer cancel()

	const query = `
              INSERT INTO accounts (id, owner_id, kind, currency)
              VALUES ($1, $2, $3, $4)
      `
	_, err := r.pool.Exec(ctx, query, a.ID, a.OwnerID, string(a.Kind), a.Currency)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return domain.ErrAccountAlreadyExists
		}
		return fmt.Errorf("postgres: create account: %w", err)
	}
	return nil
}

// FindByID looks up an account by id. It returns domain.ErrAccountNotFound
// if no row matches.
func (r *AccountRepository) FindByID(ctx context.Context, id uuid.UUID) (app.Account, error) {
	ctx, cancel := r.pool.withAcquireTimeout(ctx)
	defer cancel()

	const query = `
              SELECT id, owner_id, kind, currency, created_at
              FROM accounts
              WHERE id = $1
      `

	var (
		a    app.Account
		kind string
	)
	err := r.pool.QueryRow(ctx, query, id).Scan(&a.ID, &a.OwnerID, &kind, &a.Currency, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.Account{}, domain.ErrAccountNotFound
		}
		return app.Account{}, fmt.Errorf("postgres: find account: %w", err)
	}
	a.Kind = domain.AccountKind(kind)

	return a, nil
}

// EnsureSystem makes sure the single system account exists and returns its
// id. It is safe to call on every startup and from concurrent instances.
func (r *AccountRepository) EnsureSystem(ctx context.Context) (uuid.UUID, error) {
	ctx, cancel := r.pool.withAcquireTimeout(ctx)
	defer cancel()

	const insert = `
              INSERT INTO accounts (id, owner_id, kind, currency)
              VALUES ($1, 'system', 'system', 'BRL')
              ON CONFLICT (owner_id, kind, currency) DO NOTHING
      `
	if _, err := r.pool.Exec(ctx, insert, uuid.New()); err != nil {
		return uuid.Nil, fmt.Errorf("postgres: ensure system account: %w", err)
	}

	const selectID = `SELECT id FROM accounts WHERE owner_id = 'system' AND kind = 'system' AND currency = 'BRL'`
	var id uuid.UUID
	if err := r.pool.QueryRow(ctx, selectID).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("postgres: read system account: %w", err)
	}
	return id, nil
}
