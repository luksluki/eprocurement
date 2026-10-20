// Package config loads identity service configuration from the environment.
package config

import (
	"os"
)

// Config holds the identity service runtime configuration.
type Config struct {
	Env         string // APP_ENV (default "local")
	Port        string // PORT — gRPC listen port (default "8081")
	DatabaseURL string // DATABASE_URL — PostgreSQL connection string
	LogLevel    string // LOG_LEVEL (default "info")
}

// Load reads the configuration from the environment. Every value has a
// local-development default, so the service starts on a plain compose stack
// without any env file beyond DATABASE_URL's default.
func Load() Config {
	return Config{
		Env:         envOr("APP_ENV", "local"),
		Port:        envOr("PORT", "8081"),
		DatabaseURL: envOr("DATABASE_URL", "postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable"),
		LogLevel:    envOr("LOG_LEVEL", "info"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
