package postgres

import (
	"context"
	"fmt"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Entry is the row shape of the entries table.
type Entry struct {
	TransferID uuid.UUID
	AccountID  uuid.UUID
	// AmountCents follows entries.amount_cents: positive credits the
	// account, negative debits it. Never zero (enforced by the DB CHECK).
	AmountCents int64
}

// EntryRepository reads and writes the entries table the ledger itself.
type EntryRepository struct {
	pool *Pool
}

// NewEntryRepository builds an EntryRepository backed by pool.
func NewEntryRepository(pool *Pool) *EntryRepository {
	return &EntryRepository{pool: pool}
}

// InsertBatch inserts all of entries in a single round trip using pgx's
// batch/pipelining support. It does not open its own transaction — callers
// that need the insert to be atomic with other statements (e.g. the
// transfer row itself, in Sessão 3) are responsible for wrapping the call
// in a pgx.Tx and passing that as part of ctx/pool.
func (r *EntryRepository) InsertBatch(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}

	ctx, cancel := r.pool.withAcquireTimeout(ctx)
	defer cancel()

	const query = `
              INSERT INTO entries (transfer_id, account_id, amount_cents)
              VALUES ($1, $2, $3)
      `
	batch := &pgx.Batch{}
	for _, e := range entries {
		batch.Queue(query, e.TransferID, e.AccountID, e.AmountCents)
	}

	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()

	for range entries {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("postgres: insert entry: %w", err)
		}
	}

	return nil
}

// SumByAccount derives an account's current balance: the sum of all its
// entries. There is no stored balance column — this query IS the balance,
// per the ledger model in docs/escopo-carteira-go.md §1.
func (r *EntryRepository) SumByAccount(ctx context.Context, accountID uuid.UUID) (domain.Money, error) {
	ctx, cancel := r.pool.withAcquireTimeout(ctx)
	defer cancel()

	const query = `
              SELECT COALESCE(SUM(amount_cents), 0)
              FROM entries
              WHERE account_id = $1
      `

	var sum int64
	if err := r.pool.QueryRow(ctx, query, accountID).Scan(&sum); err != nil {
		return 0, fmt.Errorf("postgres: sum entries: %w", err)
	}

	return domain.NewMoney(sum), nil
}
