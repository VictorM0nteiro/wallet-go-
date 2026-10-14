package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/VictorM0nteiro/wallet-go/internal/adapters/postgres"
)

type config struct {
	dsn          string
	pollInterval time.Duration
	batchSize    int
}

func loadConfig() (config, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return config{}, errors.New("DATABASE_URL must be set")
	}
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return config{}, fmt.Errorf("DATABASE_URL is invalid: %w", err)
	}

	pollInterval := time.Second
	if v := os.Getenv("RELAY_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("RELAY_POLL_INTERVAL must be a positive duration, got %q", v)
		}
		pollInterval = d
	}
	batchSize := 20
	if v := os.Getenv("RELAY_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return config{}, fmt.Errorf("RELAY_BATCH_SIZE must be a positive integer, got %q", v)
		}
		batchSize = n
	}

	return config{dsn: dsn, pollInterval: pollInterval, batchSize: batchSize}, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("relay: fatal", "err", err)
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
		MaxConns:        5,
		MaxConnLifetime: 30 * time.Minute,
		AcquireTimeout:  3 * time.Second,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	relay := postgres.NewOutboxRelay(pool)
	publisher := postgres.LogPublisher{Logger: logger}

	logger.Info("relay: polling", "poll_interval", cfg.pollInterval, "batch_size", cfg.batchSize)
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("relay: shutting down")
			return nil
		case <-ticker.C:
			published, err := relay.PollOnce(ctx, publisher, cfg.batchSize)
			if err != nil {
				logger.Error("relay: poll failed", "err", err)
				continue
			}
			if published > 0 {
				logger.Info("relay: published", "count", published)
			}
		}
	}
}
