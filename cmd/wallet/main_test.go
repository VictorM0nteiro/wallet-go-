package main

import "testing"

// setMinimalEnv sets the variables loadConfig requires, so each test only
// has to set the ones it cares about.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable")
	t.Setenv("WALLET_API_KEY", "write-key")
	t.Setenv("WALLET_API_KEY_READONLY", "")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("WALLET_TRANSFER_STRATEGY", "")
	t.Setenv("WALLET_DB_MAX_CONNS", "")
	t.Setenv("WALLET_RATE_LIMIT_RPS", "")
	t.Setenv("WALLET_RATE_LIMIT_BURST", "")
}

func TestLoadConfig_ReadOnlyAPIKeyIsOptional(t *testing.T) {
	setMinimalEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want nil", err)
	}
	if cfg.readOnlyAPIKey != "" {
		t.Errorf("readOnlyAPIKey = %q, want empty when WALLET_API_KEY_READONLY is unset", cfg.readOnlyAPIKey)
	}
}

func TestLoadConfig_ReadOnlyAPIKeyIsAccepted(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("WALLET_API_KEY_READONLY", "read-key")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want nil", err)
	}
	if cfg.readOnlyAPIKey != "read-key" {
		t.Errorf("readOnlyAPIKey = %q, want %q", cfg.readOnlyAPIKey, "read-key")
	}
	if cfg.apiKey != "write-key" {
		t.Errorf("apiKey = %q, want %q", cfg.apiKey, "write-key")
	}
}

func TestLoadConfig_ReadOnlyAPIKeyMustDifferFromWriteKey(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("WALLET_API_KEY_READONLY", "write-key") // same as WALLET_API_KEY

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error when both keys are equal")
	}
}

func TestLoadConfig_RateLimitDefaults(t *testing.T) {
	setMinimalEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want nil", err)
	}
	if cfg.rateLimitRPS != 50 {
		t.Errorf("rateLimitRPS = %v, want 50", cfg.rateLimitRPS)
	}
	if cfg.rateLimitBurst != 100 {
		t.Errorf("rateLimitBurst = %v, want 100", cfg.rateLimitBurst)
	}
}

func TestLoadConfig_RateLimitZeroDisablesIt(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("WALLET_RATE_LIMIT_RPS", "0")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want nil", err)
	}
	if cfg.rateLimitRPS != 0 {
		t.Errorf("rateLimitRPS = %v, want 0", cfg.rateLimitRPS)
	}
}

func TestLoadConfig_RateLimitCustomValues(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("WALLET_RATE_LIMIT_RPS", "10.5")
	t.Setenv("WALLET_RATE_LIMIT_BURST", "20")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want nil", err)
	}
	if cfg.rateLimitRPS != 10.5 {
		t.Errorf("rateLimitRPS = %v, want 10.5", cfg.rateLimitRPS)
	}
	if cfg.rateLimitBurst != 20 {
		t.Errorf("rateLimitBurst = %v, want 20", cfg.rateLimitBurst)
	}
}

func TestLoadConfig_RateLimitRPSInvalid(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("WALLET_RATE_LIMIT_RPS", "not-a-number")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error for a non-numeric WALLET_RATE_LIMIT_RPS")
	}
}

func TestLoadConfig_RateLimitBurstMustBePositiveWhenEnabled(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("WALLET_RATE_LIMIT_RPS", "50")
	t.Setenv("WALLET_RATE_LIMIT_BURST", "0")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want an error: burst 0 with a nonzero RPS would reject every request")
	}
}
