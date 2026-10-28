// Package config loads gateway service configuration from the environment.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the gateway runtime configuration.
type Config struct {
	Env             string        // APP_ENV (default "local")
	Port            string        // PORT — HTTP listen port (default "8080")
	IdentityAddr    string        // IDENTITY_ADDR — identity gRPC host:port
	ProcurementAddr string        // PROCUREMENT_ADDR — procurement gRPC host:port
	DocumentsAddr   string        // DOCUMENTS_ADDR — documents gRPC host:port
	RabbitMQURL     string        // RABBITMQ_URL — AMQP broker (the admin DLQ surface, #48)
	JWKSCacheTTL    time.Duration // JWKS_CACHE_TTL (default 5m)
	LogLevel        string        // LOG_LEVEL (default "info")
	LoginRatePerMin int           // GATEWAY_LOGIN_RATE_PER_MIN — per-IP login limiter budget (default 30)
}

// Load reads the configuration from the environment.
func Load() Config {
	return Config{
		Env:             envOr("APP_ENV", "local"),
		Port:            envOr("PORT", "8080"),
		IdentityAddr:    envOr("IDENTITY_ADDR", "localhost:8081"),
		ProcurementAddr: envOr("PROCUREMENT_ADDR", "localhost:8082"),
		DocumentsAddr:   envOr("DOCUMENTS_ADDR", "localhost:8083"),
		RabbitMQURL:     envOr("RABBITMQ_URL", "amqp://eprocurement:eprocurement@localhost:5672/"),
		JWKSCacheTTL:    envDuration("JWKS_CACHE_TTL", 5*time.Minute),
		LogLevel:        envOr("LOG_LEVEL", "info"),
		LoginRatePerMin: envInt("GATEWAY_LOGIN_RATE_PER_MIN", 30),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
