package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const migrationsPath = "file://../../../migrations"

// newTestPool starts a throwaway Postgres container, applies every
// migration in migrations/, and returns a ready-to-use *Pool. The
// container and the pool are torn down automatically via t.Cleanup.
func newTestPool(t *testing.T) *Pool {
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

	return pool
}

func applyMigrations(dsn string) error {
	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
