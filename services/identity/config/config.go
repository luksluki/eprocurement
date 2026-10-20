// Package config loads identity service configuration from the environment.
package config

import (
	"os"
	"strconv"
)

// Config holds the identity service runtime configuration.
type Config struct {
	Env             string // APP_ENV (default "local")
	Port            string // PORT — gRPC listen port (default "8081")
	DatabaseURL     string // DATABASE_URL — PostgreSQL connection string
	LogLevel        string // LOG_LEVEL (default "info")
	JWTKeySeed      string // IDENTITY_JWT_KEY — base64 32-byte Ed25519 seed; empty = ephemeral per boot
	LoginRatePerMin int    // IDENTITY_LOGIN_RATE_PER_MIN — login attempts per email+address (default 5)
}

// Load reads the configuration from the environment. Every value has a
// local-development default, so the service starts on a plain compose stack
// without any env file beyond DATABASE_URL's default.
func Load() Config {
	return Config{
		Env:             envOr("APP_ENV", "local"),
		Port:            envOr("PORT", "8081"),
		DatabaseURL:     envOr("DATABASE_URL", "postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable"),
		LogLevel:        envOr("LOG_LEVEL", "info"),
		JWTKeySeed:      os.Getenv("IDENTITY_JWT_KEY"),
		LoginRatePerMin: envInt("IDENTITY_LOGIN_RATE_PER_MIN", 5),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
