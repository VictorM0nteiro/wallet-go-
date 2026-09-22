package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	httpapi "github.com/VictorM0nteiro/wallet-go/internal/adapters/http"
	"github.com/VictorM0nteiro/wallet-go/internal/adapters/postgres"
	"github.com/VictorM0nteiro/wallet-go/internal/app"
)

func main() {
	if err := run(); err != nil {
		slog.Error("wallet: fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// .env is optional and for local development only: if it is missing
	// (production, CI), the process just reads from the real environment.
	_ = godotenv.Load()

	dsn, apiKey := os.Getenv("DATABASE_URL"), os.Getenv("WALLET_API_KEY")
	if dsn == "" || apiKey == "" {
		return errors.New("DATABASE_URL and WALLET_API_KEY must be set")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{
		DSN:             dsn,
		MaxConns:        10,
		MaxConnLifetime: 30 * time.Minute,
		AcquireTimeout:  3 * time.Second,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	accountRepo := postgres.NewAccountRepository(pool)
	entryRepo := postgres.NewEntryRepository(pool)
	systemID, err := accountRepo.EnsureSystem(ctx)
	if err != nil {
		return err
	}

	transfers := app.NewTransferService(postgres.NewLockingTransferExecutor(pool))
	srv := &http.Server{
		Addr: addr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Accounts:       app.NewAccountService(accountRepo, entryRepo),
			Cash:           app.NewCashService(transfers, systemID),
			Transfers:      transfers,
			APIKey:         apiKey,
			RequestTimeout: 5 * time.Second,
			Logger:         logger,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("wallet: listening", "addr", addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("wallet: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
