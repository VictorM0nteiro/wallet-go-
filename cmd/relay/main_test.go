package main

import (
	"testing"
	"time"
)

// setMinimalEnv sets the variables loadConfig requires, so each test only
// has to set the ones it cares about.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://wallet_relay:wallet_relay_dev@localhost:5432/wallet?sslmode=disable")
	t.Setenv("RELAY_POLL_INTERVAL", "")
	t.Setenv("RELAY_BATCH_SIZE", "")
}

func TestLoadConfig_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("RELAY_POLL_INTERVAL", "")
	t.Setenv("RELAY_BATCH_SIZE", "")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error when DATABASE_URL is unset")
	}
}

func TestLoadConfig_RejectsInvalidDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "not-a-valid-dsn")
	t.Setenv("RELAY_POLL_INTERVAL", "")
	t.Setenv("RELAY_BATCH_SIZE", "")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error for an invalid DATABASE_URL")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want nil", err)
	}
	if cfg.pollInterval != time.Second {
		t.Errorf("pollInterval = %v, want 1s", cfg.pollInterval)
	}
	if cfg.batchSize != 20 {
		t.Errorf("batchSize = %v, want 20", cfg.batchSize)
	}
}

func TestLoadConfig_CustomValues(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("RELAY_POLL_INTERVAL", "5s")
	t.Setenv("RELAY_BATCH_SIZE", "50")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want nil", err)
	}
	if cfg.pollInterval != 5*time.Second {
		t.Errorf("pollInterval = %v, want 5s", cfg.pollInterval)
	}
	if cfg.batchSize != 50 {
		t.Errorf("batchSize = %v, want 50", cfg.batchSize)
	}
}

func TestLoadConfig_RejectsNonPositivePollInterval(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("RELAY_POLL_INTERVAL", "0s")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error for a non-positive RELAY_POLL_INTERVAL")
	}
}

func TestLoadConfig_RejectsInvalidPollInterval(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("RELAY_POLL_INTERVAL", "not-a-duration")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error for a malformed RELAY_POLL_INTERVAL")
	}
}

func TestLoadConfig_RejectsNonPositiveBatchSize(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("RELAY_BATCH_SIZE", "0")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error for a non-positive RELAY_BATCH_SIZE")
	}
}
