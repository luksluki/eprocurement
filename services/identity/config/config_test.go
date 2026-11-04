package config

import (
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("IDENTITY_JWT_KEY", "")
	t.Setenv("IDENTITY_JWT_KEY_PREVIOUS", "")
	t.Setenv("IDENTITY_LOGIN_RATE_PER_MIN", "")

	cfg := Load()
	if cfg.Env != "local" || cfg.Port != "8081" || cfg.LogLevel != "info" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.DatabaseURL == "" || cfg.JWTKeySeed != "" || cfg.JWTKeyPrevSeed != "" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.LoginRatePerMin != 5 {
		t.Fatalf("rate default = %d, want 5", cfg.LoginRatePerMin)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("PORT", "9091")
	t.Setenv("DATABASE_URL", "postgres://override")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("IDENTITY_JWT_KEY", "key-a")
	t.Setenv("IDENTITY_JWT_KEY_PREVIOUS", "key-b")

	cfg := Load()
	if cfg.Port != "9091" || cfg.DatabaseURL != "postgres://override" || cfg.LogLevel != "debug" {
		t.Fatalf("overrides wrong: %+v", cfg)
	}
	if cfg.JWTKeySeed != "key-a" || cfg.JWTKeyPrevSeed != "key-b" {
		t.Fatalf("jwt keys wrong: %+v", cfg)
	}
}

func TestLoadRateFallsBackOnGarbage(t *testing.T) {
	for _, value := range []string{"", "not-a-number", "0", "-3"} {
		t.Setenv("IDENTITY_LOGIN_RATE_PER_MIN", value)
		if cfg := Load(); cfg.LoginRatePerMin != 5 {
			t.Errorf("rate for %q = %d, want fallback 5", value, cfg.LoginRatePerMin)
		}
	}
	t.Setenv("IDENTITY_LOGIN_RATE_PER_MIN", "42")
	if cfg := Load(); cfg.LoginRatePerMin != 42 {
		t.Fatalf("rate = %d, want 42", cfg.LoginRatePerMin)
	}
}
