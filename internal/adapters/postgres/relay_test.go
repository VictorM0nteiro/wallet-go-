package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

var errBoom = errors.New("simulated delivery failure")

// funcPublisher is a stub Publisher: it records every call and, if fn is
// set, asks fn how to answer -- letting a test script exactly which calls
// succeed or fail.
type funcPublisher struct {
	mu    sync.Mutex
	calls []Event
	fn    func(e Event) error
}

func (p *funcPublisher) Publish(_ context.Context, e Event) error {
	p.mu.Lock()
	p.calls = append(p.calls, e)
	p.mu.Unlock()
	if p.fn == nil {
		return nil
	}
	return p.fn(e)
}

func (p *funcPublisher) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func seedOutboxEvent(t *testing.T, pool *Pool, eventType string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	const q = `INSERT INTO outbox (event_id, event_type, payload) VALUES ($1, $2, '{}')`
	if _, err := pool.Exec(context.Background(), q, id, eventType); err != nil {
		t.Fatalf("seed outbox event: %v", err)
	}
	return id
}

func outboxIsPublished(t *testing.T, pool *Pool, id uuid.UUID) bool {
	t.Helper()
	var published bool
	const q = `SELECT published_at IS NOT NULL FROM outbox WHERE event_id = $1`
	if err := pool.QueryRow(context.Background(), q, id).Scan(&published); err != nil {
		t.Fatalf("read outbox row %s: %v", id, err)
	}
	return published
}

// forceRetryNow bypasses the backoff delay so a test doesn't have to sleep
// through it: it asserts the same database-level redelivery PollOnce would
// eventually perform on its own, just without waiting for the clock.
func forceRetryNow(t *testing.T, pool *Pool, id uuid.UUID) {
	t.Helper()
	const q = `UPDATE outbox SET next_attempt_at = now() WHERE event_id = $1`
	if _, err := pool.Exec(context.Background(), q, id); err != nil {
		t.Fatalf("force retry on %s: %v", id, err)
	}
}

func TestOutboxRelay_PollOncePublishesAllPending(t *testing.T) {
	pool := newTestPool(t)
	relay := NewOutboxRelay(pool)

	const n = 5
	ids := make([]uuid.UUID, n)
	for i := range n {
		ids[i] = seedOutboxEvent(t, pool, "transfer.completed")
	}

	pub := &funcPublisher{}
	published, err := relay.PollOnce(context.Background(), pub, n)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if published != n {
		t.Errorf("published = %d, want %d", published, n)
	}
	if pub.callCount() != n {
		t.Errorf("Publish called %d times, want %d", pub.callCount(), n)
	}
	for _, id := range ids {
		if !outboxIsPublished(t, pool, id) {
			t.Errorf("event %s not marked published", id)
		}
	}
}

// This is X5's own acceptance test: interrupt the relay between publishing
// and marking, restart, and the event must be redelivered with the same
// event_id, never lost. A Publisher that records the call and then returns
// an error drives PollOnce down the exact rollback path a hard process
// kill would: nothing commits, so even the fact that Publish ran is not
// persisted.
func TestOutboxRelay_FailedPublishLeavesRowUnpublishedAndRedeliversSameEventID(t *testing.T) {
	pool := newTestPool(t)
	relay := NewOutboxRelay(pool)
	id := seedOutboxEvent(t, pool, "transfer.completed")

	failing := &funcPublisher{fn: func(Event) error { return errBoom }}
	published, err := relay.PollOnce(context.Background(), failing, 1)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if published != 0 {
		t.Errorf("published = %d, want 0", published)
	}
	if failing.callCount() != 1 {
		t.Fatalf("Publish called %d times, want 1", failing.callCount())
	}
	if outboxIsPublished(t, pool, id) {
		t.Fatal("event marked published despite the failed delivery")
	}

	forceRetryNow(t, pool, id)

	succeeding := &funcPublisher{}
	published, err = relay.PollOnce(context.Background(), succeeding, 1)
	if err != nil {
		t.Fatalf("PollOnce (retry): %v", err)
	}
	if published != 1 {
		t.Errorf("published = %d, want 1", published)
	}
	if succeeding.callCount() != 1 || succeeding.calls[0].ID != id {
		t.Fatalf("redelivered event = %+v, want exactly %s", succeeding.calls, id)
	}
	if !outboxIsPublished(t, pool, id) {
		t.Error("event still not marked published after the successful retry")
	}
}

// A failure on one row must not lose or duplicate the outcome of another:
// each row commits independently, in created_at order.
func TestOutboxRelay_OneFailureInABatchDoesNotAffectOtherRows(t *testing.T) {
	pool := newTestPool(t)
	relay := NewOutboxRelay(pool)
	first := seedOutboxEvent(t, pool, "transfer.completed")
	time.Sleep(5 * time.Millisecond) // created_at must order deterministically
	second := seedOutboxEvent(t, pool, "transfer.completed")

	pub := &funcPublisher{fn: func(e Event) error {
		if e.ID == second {
			return errBoom
		}
		return nil
	}}
	published, err := relay.PollOnce(context.Background(), pub, 2)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if published != 1 {
		t.Errorf("published = %d, want 1", published)
	}
	if !outboxIsPublished(t, pool, first) {
		t.Error("first event was not published")
	}
	if outboxIsPublished(t, pool, second) {
		t.Error("second event should not be published, its delivery failed")
	}
}
