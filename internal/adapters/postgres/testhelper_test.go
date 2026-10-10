package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const migrationsPath = "file://../../../migrations"

// appRole and appRolePassword mirror deploy/postgres/init/01-app-role.sql:
// in a real environment that script creates the role before any migration
// runs, but the test container has no init script, so migration 000002's
// GRANTs would fail with "role wallet_app does not exist" without this.
const (
	appRole         = "wallet_app"
	appRolePassword = "wallet_app_dev"
)

// newTestPool starts a throwaway Postgres container, applies every
// migration in migrations/, and returns a ready-to-use *Pool. The
// container and the pool are torn down automatically via t.Cleanup.
func newTestPool(t *testing.T) *Pool {
	t.Helper()
	pool, _ := newTestPoolWithDSN(t)
	return pool
}

// newTestPoolWithDSN is newTestPool plus the owner DSN, for tests that also
// need to connect as wallet_app (see appDSN) to check its grants directly.
func newTestPoolWithDSN(t *testing.T) (*Pool, string) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("wallet_test"),
		tcpostgres.WithUsername("wallet"),
		tcpostgres.WithPassword("wallet"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("build connection string: %v", err)
	}

	if err := createAppRole(ctx, dsn); err != nil {
		t.Fatalf("create wallet_app role: %v", err)
	}
	if err := applyMigrations(dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	pool, err := NewPool(ctx, PoolConfig{
		DSN:             dsn,
		MaxConns:        5,
		MaxConnLifetime: time.Minute,
		AcquireTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool, dsn
}

// createAppRole connects as the schema owner (the DSN's own credentials) and
// creates wallet_app, the role migration 000002 grants least-privilege
// access to. A fresh container never has it until this runs.
func createAppRole(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect as schema owner: %w", err)
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD '%s'`, appRole, appRolePassword,
	))
	return err
}

// appDSN swaps the owner credentials in dsn for wallet_app's, so a test can
// connect as the least-privilege role and check its grants directly instead
// of trusting that the migration did what it says.
func appDSN(dsn string) string {
	return strings.Replace(dsn, "wallet:wallet@", appRole+":"+appRolePassword+"@", 1)
}

func applyMigrations(dsn string) error {
	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
