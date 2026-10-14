package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
)

// Same full-jitter shape as serializable_executor.go's retry backoff, just
// on a poll-interval timescale (seconds, not milliseconds) since this is
// spacing between relay attempts, not spacing between transaction retries.
const (
	relayBaseBackoff = 1 * time.Second
	relayMaxBackoff  = 60 * time.Second
)

// nextAttempt returns when a row that just failed to publish should be
// tried again: full jitter, doubling with attempts, capped at relayMaxBackoff.
func nextAttempt(attempts int) time.Time {
	ceiling := relayBaseBackoff
	if attempts > 0 {
		ceiling = relayBaseBackoff << attempts
	}
	if ceiling > relayMaxBackoff || ceiling <= 0 {
		ceiling = relayMaxBackoff
	}
	wait := time.Duration(rand.Int64N(int64(ceiling)))
	return time.Now().Add(wait)
}

// OutboxRelay delivers outbox events at least once by polling.
type OutboxRelay struct {
	pool *Pool
}

// NewOutboxRelay builds an OutboxRelay backed by pool.
func NewOutboxRelay(pool *Pool) *OutboxRelay {
	return &OutboxRelay{pool: pool}
}

// PollOnce processes up to batchSize pending events, one transaction per
// event: SELECT ... FOR UPDATE SKIP LOCKED (safe if more than one relay
// instance ever runs, even though only one runs today), Publish, then mark
// the row published -- or, on a failed Publish, record the attempt and
// schedule a retry -- all inside that same transaction, which commits only
// once that row's own outcome is decided.
//
// If the process dies after Publish returns but before the transaction
// commits, nothing for that row is persisted -- not even the fact that it
// was attempted -- so the next poll picks it up again and redelivers it
// with the same event_id. That is the at-least-once guarantee X5 asks for:
// a row may be published more than once, it is never lost.
//
// One row's outcome is isolated from the others: a failure on row 2 of a
// batch neither loses nor duplicates row 1's result, because each row has
// already committed independently by the time row 2 is even selected.
func (r *OutboxRelay) PollOnce(ctx context.Context, pub Publisher, batchSize int) (published int, err error) {
	for range batchSize {
		ok, didPublish, err := r.processOne(ctx, pub)
		if err != nil {
			return published, err
		}
		if !ok {
			break
		}
		if didPublish {
			published++
		}
	}
	return published, nil
}

// processOne claims and processes a single pending row, if one is ready.
// ok reports whether a row was found at all; published reports whether
// that row's Publish succeeded (false means the attempt was recorded and
// the row rescheduled for later).
func (r *OutboxRelay) processOne(ctx context.Context, pub Publisher) (ok, published bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("postgres: begin relay tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var e Event
	err = tx.QueryRow(ctx, `
		SELECT event_id, event_type, payload, attempts
		FROM outbox
		WHERE published_at IS NULL AND next_attempt_at <= now()
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&e.ID, &e.Type, &e.Payload, &e.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("postgres: poll outbox: %w", err)
	}

	if pubErr := pub.Publish(ctx, e); pubErr != nil {
		_, err := tx.Exec(ctx, `
			UPDATE outbox SET attempts = attempts + 1, next_attempt_at = $2
			WHERE event_id = $1
		`, e.ID, nextAttempt(e.Attempts+1))
		if err != nil {
			return false, false, fmt.Errorf("postgres: record failed attempt: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("postgres: commit relay tx: %w", err)
		}
		return true, false, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE outbox SET published_at = now(), attempts = attempts + 1
		WHERE event_id = $1
	`, e.ID); err != nil {
		return false, false, fmt.Errorf("postgres: mark event published: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("postgres: commit relay tx: %w", err)
	}
	return true, true, nil
}
