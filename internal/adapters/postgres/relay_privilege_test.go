package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestWalletRelayLeastPrivilege connects as wallet_relay itself (not the
// schema owner) and proves migration 000003 does what it claims: the relay
// can only ever touch the outbox, never accounts/transfers/entries or the
// idempotency store, regardless of how many relay instances ever run.
func TestWalletRelayLeastPrivilege(t *testing.T) {
	_, dsn := newTestPoolWithDSN(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, relayDSN(dsn))
	if err != nil {
		t.Fatalf("connect as wallet_relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	t.Run("cannot read accounts", func(t *testing.T) {
		_, err := conn.Exec(ctx, `SELECT count(*) FROM accounts`)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot read transfers", func(t *testing.T) {
		_, err := conn.Exec(ctx, `SELECT count(*) FROM transfers`)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot read entries", func(t *testing.T) {
		_, err := conn.Exec(ctx, `SELECT count(*) FROM entries`)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot read idempotency_keys", func(t *testing.T) {
		_, err := conn.Exec(ctx, `SELECT count(*) FROM idempotency_keys`)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot insert into outbox", func(t *testing.T) {
		_, err := conn.Exec(ctx,
			`INSERT INTO outbox (event_id, event_type, payload) VALUES (gen_random_uuid(), 'x', '{}')`)
		assertPermissionDenied(t, err)
	})

	t.Run("can select and take FOR UPDATE SKIP LOCKED on outbox", func(t *testing.T) {
		rows, err := conn.Query(ctx,
			`SELECT event_id FROM outbox WHERE event_id = gen_random_uuid() FOR UPDATE SKIP LOCKED`)
		if err != nil {
			t.Fatalf("wallet_relay should be able to poll outbox: %v", err)
		}
		rows.Close()
	})

	t.Run("can update published_at and attempts within its grants", func(t *testing.T) {
		_, err := conn.Exec(ctx,
			`UPDATE outbox SET published_at = now(), attempts = attempts + 1 WHERE event_id = gen_random_uuid()`)
		if err != nil {
			t.Fatalf("wallet_relay should be able to mark outbox rows published: %v", err)
		}
	})
}
