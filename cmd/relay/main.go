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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/VictorM0nteiro/wallet-go/internal/adapters/postgres"
)

type config struct {
	dsn          string
	pollInterval time.Duration
	batchSize    int
	metricsAddr  string
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

	metricsAddr := os.Getenv("RELAY_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9091"
	}
	if !strings.Contains(metricsAddr, ":") {
		return config{}, fmt.Errorf("RELAY_METRICS_ADDR must be host:port or :port, got %q", metricsAddr)
	}

	return config{dsn: dsn, pollInterval: pollInterval, batchSize: batchSize, metricsAddr: metricsAddr}, nil
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

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	relayMetrics := postgres.NewRelayMetrics(registry)
	// wallet_relay is the only role with SELECT on outbox (migration
	// 000003_outbox.up.sql): wallet_app, which cmd/wallet connects as, can
	// only INSERT into it. The lag gauges live here, not in cmd/wallet's
	// registry, for that reason -- not just because "any pool could run the
	// query" in the abstract.
	postgres.RegisterOutboxLag(registry, pool, logger)

	relay := postgres.NewOutboxRelay(pool).WithMetrics(relayMetrics)
	publisher := postgres.LogPublisher{Logger: logger}

	metricsSrv := &http.Server{
		Addr:              cfg.metricsAddr,
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	metricsErrCh := make(chan error, 1)
	go func() { metricsErrCh <- metricsSrv.ListenAndServe() }()

	logger.Info("relay: polling", "poll_interval", cfg.pollInterval, "batch_size", cfg.batchSize,
		"metrics_addr", cfg.metricsAddr)
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("relay: shutting down")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("metrics server graceful shutdown: %w", err)
			}
			return nil
		case err := <-metricsErrCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("metrics server: %w", err)
			}
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
