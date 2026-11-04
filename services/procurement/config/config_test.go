package config

import (
	"testing"
)

// Config parsing unit tests (#57), mirroring services/identity/config:
// defaults on an empty environment, overrides, and envOr's empty-value
// fallback — all through Load so the exported surface stays the contract.
func TestLoadDefaults(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("METRICS_PORT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	cfg := Load()
	if cfg.Env != "local" || cfg.Port != "8082" || cfg.LogLevel != "info" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.MetricsPort != "9102" {
		t.Fatalf("metrics port = %q, want 9102", cfg.MetricsPort)
	}
	if cfg.DatabaseURL == "" {
		t.Fatalf("database url default is empty")
	}
	if cfg.OTLPEndpoint != "" {
		t.Fatalf("otlp endpoint = %q, want empty", cfg.OTLPEndpoint)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	t.Setenv("PORT", "9092")
	t.Setenv("DATABASE_URL", "postgres://override")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("METRICS_PORT", "9199")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "otel:4317")

	cfg := Load()
	if cfg.Env != "staging" || cfg.Port != "9092" || cfg.LogLevel != "debug" {
		t.Fatalf("overrides wrong: %+v", cfg)
	}
	if cfg.MetricsPort != "9199" || cfg.OTLPEndpoint != "otel:4317" {
		t.Fatalf("overrides wrong: %+v", cfg)
	}
	if cfg.DatabaseURL != "postgres://override" {
		t.Fatalf("database url = %q, want override", cfg.DatabaseURL)
	}
}

// envOr falls back whenever the variable is unset or empty — an exported
// empty string must not shadow the default.
func TestLoadEmptyValueFallsBack(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("LOG_LEVEL", "   ")

	cfg := Load()
	if cfg.Port != "8082" {
		t.Fatalf("empty PORT = %q, want default 8082", cfg.Port)
	}
	if cfg.LogLevel != "   " {
		t.Fatalf("blank LOG_LEVEL = %q, want it kept (envOr only falls back on empty)", cfg.LogLevel)
	}
}
