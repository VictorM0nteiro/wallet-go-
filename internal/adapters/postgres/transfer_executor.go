package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LockingTransferExecutor is concurrency strategy 1: pessimistic row locks.
// It implements app.TransferExecutor.
type LockingTransferExecutor struct {
	pool *Pool
}

// NewLockingTransferExecutor builds a LockingTransferExecutor backed by pool.
func NewLockingTransferExecutor(pool *Pool) *LockingTransferExecutor {
	return &LockingTransferExecutor{pool: pool}
}

// Execute runs the whole transfer in one transaction: lock both accounts in
// ascending id order, read the source balance, ask the domain whether the
// debit is allowed, then insert the transfer and its two entries. Any error
// rolls everything back, so a rejected transfer leaves nothing behind.
func (e *LockingTransferExecutor) Execute(ctx context.Context, req app.TransferRequest) error {
	ctx, cancel := e.pool.withAcquireTimeout(ctx)
	defer cancel()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin transfer tx: %w", err)
	}
	// After a successful Commit this is a no-op that returns pgx.ErrTxClosed.
	defer func() { _ = tx.Rollback(ctx) }()

	kinds := make(map[uuid.UUID]domain.AccountKind, 2)
	for _, id := range lockOrder(req.FromAccountID, req.ToAccountID) {
		kind, err := lockAccount(ctx, tx, id)
		if err != nil {
			return err
		}
		kinds[id] = kind
	}

	// The row lock on the source account serializes every transfer touching
	// it, so this sum cannot change under us until we commit.
	var balance int64
	const sumQuery = `SELECT COALESCE(SUM(amount_cents), 0) FROM entries WHERE account_id = $1`
	if err := tx.QueryRow(ctx, sumQuery, req.FromAccountID).Scan(&balance); err != nil {
		return fmt.Errorf("postgres: read source balance: %w", err)
	}

	if err := domain.CanDebit(domain.NewMoney(balance), req.Amount, kinds[req.FromAccountID]); err != nil {
		return err
	}

	const insertTransfer = `
              INSERT INTO transfers (id, from_account_id, to_account_id, amount_cents, status)
              VALUES ($1, $2, $3, $4, 'completed')
      `
	if _, err := tx.Exec(ctx, insertTransfer, req.ID, req.FromAccountID, req.ToAccountID, req.Amount.Cents()); err != nil {
		return fmt.Errorf("postgres: insert transfer: %w", err)
	}

	const insertEntry = `INSERT INTO entries (transfer_id, account_id, amount_cents) VALUES ($1, $2, $3)`
	if _, err := tx.Exec(ctx, insertEntry, req.ID, req.FromAccountID, -req.Amount.Cents()); err != nil {
		return fmt.Errorf("postgres: insert debit entry: %w", err)
	}
	if _, err := tx.Exec(ctx, insertEntry, req.ID, req.ToAccountID, req.Amount.Cents()); err != nil {
		return fmt.Errorf("postgres: insert credit entry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit transfer: %w", err)
	}
	return nil
}

// lockOrder returns the two ids in ascending order. Postgres orders uuid
// bytewise, so bytes.Compare matches ORDER BY id. Always locking in this
// order is what prevents A->B and B->A from deadlocking.
func lockOrder(a, b uuid.UUID) [2]uuid.UUID {
	if bytes.Compare(a[:], b[:]) <= 0 {
		return [2]uuid.UUID{a, b}
	}
	return [2]uuid.UUID{b, a}
}

func lockAccount(ctx context.Context, tx pgx.Tx, id uuid.UUID) (domain.AccountKind, error) {
	var kind string
	err := tx.QueryRow(ctx, `SELECT kind FROM accounts WHERE id = $1 FOR UPDATE`, id).Scan(&kind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.ErrAccountNotFound
		}
		return "", fmt.Errorf("postgres: lock account: %w", err)
	}
	return domain.AccountKind(kind), nil
}
