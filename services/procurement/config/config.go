// Package config loads procurement service configuration from the environment.
package config

import (
	"os"
)

// Config holds the procurement service runtime configuration.
type Config struct {
	Env         string // APP_ENV (default "local")
	Port        string // PORT — gRPC listen port (default "8082")
	DatabaseURL string // DATABASE_URL — PostgreSQL connection string
	LogLevel    string // LOG_LEVEL (default "info")
	MetricsPort string // METRICS_PORT — /metrics listen port, container-internal (default "9102", #55)

	OTLPEndpoint string // OTEL_EXPORTER_OTLP_ENDPOINT — OTLP gRPC collector for traces (#54); empty = SDK active, nothing exported
}

// Load reads the configuration from the environment with local-development
// defaults.
func Load() Config {
	return Config{
		Env:          envOr("APP_ENV", "local"),
		Port:         envOr("PORT", "8082"),
		DatabaseURL:  envOr("DATABASE_URL", "postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable"),
		LogLevel:     envOr("LOG_LEVEL", "info"),
		MetricsPort:  envOr("METRICS_PORT", "9102"),
		OTLPEndpoint: envOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
