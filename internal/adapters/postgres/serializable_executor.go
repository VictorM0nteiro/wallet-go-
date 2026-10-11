package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ app.TransferExecutor = (*SerializableTransferExecutor)(nil)

const (
	deafaultMaxAttempts = 10
	baseBackoff         = 5 * time.Millisecond
	maxBackoff          = 200 * time.Millisecond
)

// SerializableTransferExecutor is concurrency strategy 2: no explicit row
// locks. Every transaction runs at SERIALIZABLE, and Postgres aborts one of
// two conflicting transactions with SQLSTATE 40001 (or 40P01 for deadlock).
// The executor retries the whole transaction in that case, so fn is called
// again and must re-read everything it depends on.
type SerializableTransferExecutor struct {
	pool        *Pool
	maxAttempts int
	retries     atomic.Uint64
}

func NewSerializableTransferExecutor(pool *Pool) *SerializableTransferExecutor {
	return &SerializableTransferExecutor{pool: pool, maxAttempts: deafaultMaxAttempts}
}

// Retries reports how many serialization failures were retried since the
// executor was built. It is the counter the execution plan asks to export.
func (e *SerializableTransferExecutor) Retries() uint64 {
	return e.retries.Load()
}

func (e *SerializableTransferExecutor) InTx(ctx context.Context, fn func(context.Context, app.TransferTx) error) error {
	ctx, cancel := e.pool.withAcquireTimeout(ctx)
	defer cancel()

	for attempt := 1; ; attempt++ {
		err := e.attempt(ctx, fn)
		if !isSerializationFailure(err) {
			return err
		}
		if attempt >= e.maxAttempts {
			return fmt.Errorf("postgres: serializable transfer gave up after %d attempts: %w: %w",
				attempt, app.ErrConcurrencyConflict, err)
		}
		e.retries.Add(1)
		if err := sleepBackoff(ctx, attempt); err != nil {
			return err
		}
	}
}

func (e *SerializableTransferExecutor) attempt(ctx context.Context, fn func(context.Context, app.TransferTx) error) error {
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("postgres: begin serializable tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, &serializableTx{lockingTx{tx: tx}}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit serializable tx: %w", err)
	}
	return nil
}

// serializableTx reuses the idempotency methods of lockingTx and replaces
// only Execute. The difference between the two strategies is exactly here.
type serializableTx struct {
	lockingTx
}

func (s *serializableTx) Execute(ctx context.Context, req app.TransferRequest) error {
	kinds := make(map[uuid.UUID]domain.AccountKind, 2)
	for _, id := range []uuid.UUID{req.FromAccountID, req.ToAccountID} {
		kind, err := readAccountKind(ctx, s.tx, id)
		if err != nil {
			return err
		}
		kinds[id] = kind
	}

	var balance int64
	const sumQuery = `SELECT COALESCE(SUM(amount_cents), 0) FROM entries WHERE account_id = $1`
	if err := s.tx.QueryRow(ctx, sumQuery, req.FromAccountID).Scan(&balance); err != nil {
		return fmt.Errorf("postgres: read source balance: %w", err)
	}

	if err := domain.CanDebit(domain.NewMoney(balance), req.Amount, kinds[req.FromAccountID]); err != nil {
		return err
	}

	return insertMovement(ctx, s.tx, req)
}

func readAccountKind(ctx context.Context, tx pgx.Tx, id uuid.UUID) (domain.AccountKind, error) {
	var kind string
	err := tx.QueryRow(ctx, `SELECT kind FROM accounts WHERE id = $1`, id).Scan(&kind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.ErrAccountNotFound
		}
		return "", fmt.Errorf("postgres: read account: %w", err)
	}
	return domain.AccountKind(kind), nil
}

func insertMovement(ctx context.Context, tx pgx.Tx, req app.TransferRequest) error {
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
	return nil
}

// isSerializationFailure matches only the retryable SQLSTATEs. Domain errors
// such as insufficient funds are not retried, and neither is a deadline.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

// sleepBackoff waits a random time up to an exponentially growing ceiling
// (full jitter), so retrying transactions do not collide again in lockstep.
func sleepBackoff(ctx context.Context, attempt int) error {
	ceiling := baseBackoff << (attempt - 1)
	if ceiling > maxBackoff || ceiling <= 0 {
		ceiling = maxBackoff
	}
	wait := time.Duration(rand.Int64N(int64(ceiling))) //nolint:gosec // jitter does not need crypto randomness
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Execute runs a single transfer with no idempotency handling, like the
// locking executor. It exists so the test suite can seed balances directly.
func (e *SerializableTransferExecutor) Execute(ctx context.Context, req app.TransferRequest) error {
	return e.InTx(ctx, func(ctx context.Context, tx app.TransferTx) error {
		return tx.Execute(ctx, req)
	})
}

// O que cada parte faz:

// - InTx com retry. Cada tentativa abre uma transação nova e roda fn do zero. Se o erro for 40001 ou 40P01,
// a tentativa é descartada e tenta de novo, até maxAttempts. Qualquer outro erro, como saldo insuficiente, volta direto, sem retry.
// - serializableTx embute lockingTx. ClaimKey, LoadKey e CompleteKey são idênticos nas duas estratégias,
// porque a idempotência não é o que estamos comparando. Só o Execute muda.
// - Execute sem FOR UPDATE. A leitura do saldo e a checagem de existência acontecem dentro da transação SERIALIZABLE.
// O Postgres detecta quando duas transações lêem e escrevem sobre os mesmos dados, e aborta uma delas no commit.
// É nisso que a estratégia 2 confia, em vez de travar a linha.
// - Retry refaz a leitura. Como fn roda do início a cada tentativa, o saldo é lido de novo. Nunca reaproveitamos um valor lido antes, que é o bug que o plano avisa para evitar.
// - Retries() expõe o contador. O plano pede que ele seja exportado, e ele será usado nas medições.
// - Jitter. Sem variação aleatória, transações em conflito tentariam de novo no mesmo instante e colidiriam outra vez.
