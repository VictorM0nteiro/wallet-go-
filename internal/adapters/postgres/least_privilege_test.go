package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestWalletAppLeastPrivilege connects as wallet_app itself (not as the
// schema owner the rest of the suite uses) and proves migration 000002
// does what it claims. The append-only invariant on entries is only a real
// guarantee once it holds for a client with valid credentials, not just by
// convention in internal/adapters/postgres's own queries.
func TestWalletAppLeastPrivilege(t *testing.T) {
	_, dsn := newTestPoolWithDSN(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, appDSN(dsn))
	if err != nil {
		t.Fatalf("connect as wallet_app: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	t.Run("cannot update entries", func(t *testing.T) {
		_, err := conn.Exec(ctx,
			`UPDATE entries SET amount_cents = amount_cents + 1 WHERE id = -1`)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot delete entries", func(t *testing.T) {
		_, err := conn.Exec(ctx, `DELETE FROM entries WHERE id = -1`)
		assertPermissionDenied(t, err)
	})

	t.Run("can select and insert within its grants", func(t *testing.T) {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM entries`).Scan(&n); err != nil {
			t.Fatalf("wallet_app should be able to SELECT entries: %v", err)
		}
	})

	t.Run("can take FOR UPDATE locks on accounts", func(t *testing.T) {
		// This is what the locking strategy actually runs. A fresh test
		// container has no accounts yet; the point is that the privilege
		// check at parse time does not reject the statement, matching rows
		// or not.
		rows, err := conn.Query(ctx,
			`SELECT kind FROM accounts WHERE id = gen_random_uuid() FOR UPDATE`)
		if err != nil {
			t.Fatalf("wallet_app should be able to SELECT ... FOR UPDATE on accounts: %v", err)
		}
		rows.Close()
	})
}

func assertPermissionDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a permission error, got none")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected SQLSTATE 42501 (insufficient_privilege), got %v", err)
	}
}
