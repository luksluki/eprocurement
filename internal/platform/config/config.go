package config

import "os"

// Config holds the minimal runtime configuration shared by all services.
type Config struct {
	Service string
	Env     string
	Port    string
}

// Load returns the configuration for a service, reading environment variables with defaults.
func Load(service string) Config {
	return Config{
		Service: service,
		Env:     envOrDefault("APP_ENV", "local"),
		Port:    envOrDefault("PORT", "8080"),
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
