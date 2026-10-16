package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	httpapi "github.com/VictorM0nteiro/wallet-go/internal/adapters/http"
	"github.com/VictorM0nteiro/wallet-go/internal/adapters/postgres"
	"github.com/VictorM0nteiro/wallet-go/internal/app"
)

type config struct {
	dsn            string
	apiKey         string
	readOnlyAPIKey string
	addr           string
	strategy       string
	maxConns       int32
	rateLimitRPS   float64
	rateLimitBurst int
}

func loadConfig() (config, error) {
	dsn, apiKey := os.Getenv("DATABASE_URL"), os.Getenv("WALLET_API_KEY")
	if dsn == "" || apiKey == "" {
		return config{}, errors.New("DATABASE_URL and WALLET_API_KEY must be set")
	}
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return config{}, fmt.Errorf("DATABASE_URL is invalid: %w", err)
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if !strings.Contains(addr, ":") {
		return config{}, fmt.Errorf("LISTEN_ADDR must be host:port or :port, got %q", addr)
	}
	strategy := os.Getenv("WALLET_TRANSFER_STRATEGY")
	if strategy == "" {
		strategy = "locking"
	}
	if strategy != "locking" && strategy != "serializable" {
		return config{}, fmt.Errorf("WALLET_TRANSFER_STRATEGY must be locking or serializable, got %q", strategy)
	}

	maxConns := int32(30)
	if v := os.Getenv("WALLET_DB_MAX_CONNS"); v != "" {
		// ParseInt with bitSize 32, not Atoi: Atoi returns a platform-width
		// int, and truncating that into int32 would silently wrap a huge
		// value instead of rejecting it. ParseInt errors with ErrRange
		// itself when v doesn't fit in 32 bits.
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			return config{}, fmt.Errorf("WALLET_DB_MAX_CONNS must be a positive integer, got %q", v)
		}
		maxConns = int32(n)
	}

	// Optional: a second key that can only read (balance, entries), rejected
	// with 403 on anything that moves money or creates an account. Empty
	// means the feature is off and WALLET_API_KEY can do everything, as
	// before.
	readOnlyAPIKey := os.Getenv("WALLET_API_KEY_READONLY")
	if readOnlyAPIKey != "" && readOnlyAPIKey == apiKey {
		return config{}, errors.New("WALLET_API_KEY_READONLY must be different from WALLET_API_KEY")
	}

	// Per-API-key token bucket (see docs/adr/0006-rate-limiting-per-api-key.md).
	// 0 disables it entirely -- used by cmd/loadtest runs that need to
	// reproduce docs/bench/RESULTS.md unthrottled.
	rateLimitRPS := 50.0
	if v := os.Getenv("WALLET_RATE_LIMIT_RPS"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			return config{}, fmt.Errorf("WALLET_RATE_LIMIT_RPS must be a non-negative number, got %q", v)
		}
		rateLimitRPS = n
	}
	rateLimitBurst := 100
	if v := os.Getenv("WALLET_RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return config{}, fmt.Errorf("WALLET_RATE_LIMIT_BURST must be an integer, got %q", v)
		}
		rateLimitBurst = n
	}
	// A burst of 0 (or negative) does not mean "disabled" -- rate.Limiter
	// would reject every single request, forever, which is a config
	// mistake, not an intentional lockout. rateLimitRPS == 0 is the one and
	// only way to disable rate limiting.
	if rateLimitRPS > 0 && rateLimitBurst <= 0 {
		return config{}, fmt.Errorf("WALLET_RATE_LIMIT_BURST must be positive when WALLET_RATE_LIMIT_RPS is set, got %d", rateLimitBurst)
	}

	return config{
		dsn: dsn, apiKey: apiKey, readOnlyAPIKey: readOnlyAPIKey,
		addr: addr, strategy: strategy, maxConns: maxConns,
		rateLimitRPS: rateLimitRPS, rateLimitBurst: rateLimitBurst,
	}, nil

}

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

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{
		DSN:             cfg.dsn,
		MaxConns:        cfg.maxConns,
		MaxConnLifetime: 30 * time.Minute,
		AcquireTimeout:  3 * time.Second,
	})
	if err != nil {
		return err
	}
	// Runs after srv.Shutdown below returns, so in-flight requests still
	// using the pool have already drained: stop accepting -> drain -> close
	// the pool -> exit.
	defer pool.Close()

	accountRepo := postgres.NewAccountRepository(pool)
	entryRepo := postgres.NewEntryRepository(pool)
	systemID, err := accountRepo.EnsureSystem(ctx)
	if err != nil {
		return err
	}

	executor, err := postgres.NewTransferExecutor(cfg.strategy, pool)
	if err != nil {
		return err
	}
	transfers := app.NewTransferService(executor)
	srv := &http.Server{
		Addr: cfg.addr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Accounts:       app.NewAccountService(accountRepo, entryRepo),
			Cash:           app.NewCashService(transfers, systemID),
			Transfers:      transfers,
			DB:             pool,
			APIKey:         cfg.apiKey,
			ReadOnlyAPIKey: cfg.readOnlyAPIKey,
			RateLimitRPS:   cfg.rateLimitRPS,
			RateLimitBurst: cfg.rateLimitBurst,
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
	logger.Info("wallet: listening", "addr", cfg.addr, "transfer_strategy", cfg.strategy, "db_max_conns", cfg.maxConns,
		"rate_limit_rps", cfg.rateLimitRPS, "rate_limit_burst", cfg.rateLimitBurst)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("wallet: shutting down")
	if r, ok := executor.(interface{ Retries() uint64 }); ok {
		logger.Info("wallet: serialization retries", "count", r.Retries())
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
