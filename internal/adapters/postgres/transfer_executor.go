package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

var _ app.TransferExecutor = (*LockingTransferExecutor)(nil)

// LockingTransferExecutor is concurrency strategy 1: pessimistic row locks.
type LockingTransferExecutor struct {
	pool *Pool
}

// NewLockingTransferExecutor builds a LockingTransferExecutor backed by pool.
func NewLockingTransferExecutor(pool *Pool) *LockingTransferExecutor {
	return &LockingTransferExecutor{pool: pool}
}

// InTx runs fn inside one transaction and commits if fn succeeds. Any error
// rolls everything back.
func (e *LockingTransferExecutor) InTx(ctx context.Context, fn func(context.Context, app.TransferTx) error) error {
	ctx, cancel := e.pool.withAcquireTimeout(ctx)
	defer cancel()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin tx: %w", err)
	}
	// After a successful Commit this is a no-op that returns pgx.ErrTxClosed.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, &lockingTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit tx: %w", err)
	}
	return nil
}

// Execute runs a single transfer with no idempotency handling. The use case
// does not call it; it exists so tests can seed balances directly.
func (e *LockingTransferExecutor) Execute(ctx context.Context, req app.TransferRequest) error {
	return e.InTx(ctx, func(ctx context.Context, tx app.TransferTx) error {
		return tx.Execute(ctx, req)
	})
}

type lockingTx struct {
	tx pgx.Tx
}

// ClaimKey relies on the unique primary key: a concurrent claim of the same
// key blocks until the first transaction finishes, then finds the row taken.
func (l *lockingTx) ClaimKey(ctx context.Context, key app.IdempotencyKey, fingerprint string) (bool, error) {
	const query = `
              INSERT INTO idempotency_keys (scope, endpoint, key, request_fingerprint, state)
              VALUES ($1, $2, $3, $4, 'in_flight')
              ON CONFLICT DO NOTHING
      `
	tag, err := l.tx.Exec(ctx, query, key.Scope, key.Endpoint, key.Key, fingerprint)
	if err != nil {
		return false, fmt.Errorf("postgres: claim idempotency key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (l *lockingTx) LoadKey(ctx context.Context, key app.IdempotencyKey) (app.StoredKey, error) {
	// jsonb::text normalizes whitespace and key order, so the replayed body
	// is semantically equal to the original, not byte-identical.
	const query = `
              SELECT request_fingerprint,
                     state,
                     COALESCE(status_code, 0),
                     COALESCE(response_body::text, ''),
                     COALESCE(transfer_id, '00000000-0000-0000-0000-000000000000'::uuid)
              FROM idempotency_keys
              WHERE scope = $1 AND endpoint = $2 AND key = $3
      `
	var (
		stored app.StoredKey
		status int32
		body   string
	)
	err := l.tx.QueryRow(ctx, query, key.Scope, key.Endpoint, key.Key).
		Scan(&stored.Fingerprint, &stored.State, &status, &body, &stored.TransferID)
	if err != nil {
		return app.StoredKey{}, fmt.Errorf("postgres: load idempotency key: %w", err)
	}
	stored.Response = app.StoredResponse{StatusCode: int(status), Body: []byte(body)}
	return stored, nil
}

func (l *lockingTx) CompleteKey(ctx context.Context, key app.IdempotencyKey, transferID uuid.UUID, resp app.StoredResponse) error {
	const query = `
              UPDATE idempotency_keys
              SET state = 'completed', status_code = $4, response_body = $5, transfer_id = $6
              WHERE scope = $1 AND endpoint = $2 AND key = $3
      `
	tag, err := l.tx.Exec(ctx, query, key.Scope, key.Endpoint, key.Key, resp.StatusCode, resp.Body, transferID)
	if err != nil {
		return fmt.Errorf("postgres: complete idempotency key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("postgres: complete idempotency key: %d rows updated, want 1", tag.RowsAffected())
	}
	return nil
}

// Execute locks both accounts in ascending id order, reads the source
// balance, asks the domain whether the debit is allowed, then inserts the
// transfer and its two entries.
//
// The two FOR UPDATE locks and the balance read go out as one pipelined
// batch (one network round trip), and so do the three inserts (a second
// round trip) — down from six round trips in the original one-statement-
// at-a-time version. Pipelining does not change when each statement runs:
// Postgres still executes everything sent on one connection strictly in
// the order received, so lockOrder's deadlock-avoidance guarantee (always
// lock the lower id first) holds exactly as before. Only the network wait
// between statements is removed.
func (l *lockingTx) Execute(ctx context.Context, req app.TransferRequest) error {
	ids := lockOrder(req.FromAccountID, req.ToAccountID)

	lockBatch := &pgx.Batch{}
	lockBatch.Queue(`SELECT kind FROM accounts WHERE id = $1 FOR UPDATE`, ids[0])
	lockBatch.Queue(`SELECT kind FROM accounts WHERE id = $1 FOR UPDATE`, ids[1])
	// The row locks above serialize every transfer touching these accounts,
	// so this sum cannot change under us until we commit. Queued in the
	// same batch: it only needs to run after both locks, not before.
	const sumQuery = `SELECT COALESCE(SUM(amount_cents), 0) FROM entries WHERE account_id = $1`
	lockBatch.Queue(sumQuery, req.FromAccountID)

	lockResults := l.tx.SendBatch(ctx, lockBatch)
	kinds := make(map[uuid.UUID]domain.AccountKind, 2)
	for _, id := range ids {
		var kind string
		if err := lockResults.QueryRow().Scan(&kind); err != nil {
			_ = lockResults.Close()
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAccountNotFound
			}
			return fmt.Errorf("postgres: lock account: %w", err)
		}
		kinds[id] = domain.AccountKind(kind)
	}
	var balance int64
	if err := lockResults.QueryRow().Scan(&balance); err != nil {
		_ = lockResults.Close()
		return fmt.Errorf("postgres: read source balance: %w", err)
	}
	// Must close before this connection can start the next batch.
	if err := lockResults.Close(); err != nil {
		return fmt.Errorf("postgres: lock/balance batch: %w", err)
	}

	if err := domain.CanDebit(domain.NewMoney(balance), req.Amount, kinds[req.FromAccountID]); err != nil {
		return err
	}

	insertBatch := &pgx.Batch{}
	insertBatch.Queue(`
              INSERT INTO transfers (id, from_account_id, to_account_id, amount_cents, status)
              VALUES ($1, $2, $3, $4, 'completed')
      `, req.ID, req.FromAccountID, req.ToAccountID, req.Amount.Cents())
	const insertEntry = `INSERT INTO entries (transfer_id, account_id, amount_cents) VALUES ($1, $2, $3)`
	insertBatch.Queue(insertEntry, req.ID, req.FromAccountID, -req.Amount.Cents())
	insertBatch.Queue(insertEntry, req.ID, req.ToAccountID, req.Amount.Cents())

	insertResults := l.tx.SendBatch(ctx, insertBatch)
	defer func() { _ = insertResults.Close() }()
	if _, err := insertResults.Exec(); err != nil {
		return fmt.Errorf("postgres: insert transfer: %w", err)
	}
	if _, err := insertResults.Exec(); err != nil {
		return fmt.Errorf("postgres: insert debit entry: %w", err)
	}
	if _, err := insertResults.Exec(); err != nil {
		return fmt.Errorf("postgres: insert credit entry: %w", err)
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
