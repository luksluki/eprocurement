// Package config loads worker service configuration from the environment.
//
// The one worker binary runs both background halves (dev-note 047): the
// transactional outbox publisher (#45; DATABASE_URL + the poll knobs below)
// and the Mongo projection consumers (#47; MONGO_URI/MONGO_DB). RABBITMQ_URL
// is shared. PollInterval and BatchSize are pinned by the PRD (§16: poll
// every 2 s, batch 50); the env overrides exist so tests can tick fast and so
// operators can tune a demo — production keeps the defaults.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the worker runtime configuration.
type Config struct {
	Env      string // APP_ENV (default "local")
	LogLevel string // LOG_LEVEL (default "info")

	// Publisher (#45): drains procurement.outbox to RabbitMQ.
	DatabaseURL    string        // DATABASE_URL — PostgreSQL (procurement.outbox)
	PollInterval   time.Duration // WORKER_POLL_INTERVAL (default 2s, PRD §16)
	BatchSize      int           // WORKER_BATCH_SIZE (default 50, PRD §16)
	ConfirmTimeout time.Duration // WORKER_CONFIRM_TIMEOUT (default 5s)

	// Shared broker.
	RabbitMQURL string // RABBITMQ_URL — AMQP broker

	// Consumers (#47): Mongo projections (documents.history + notifications).
	MongoURI string // MONGO_URI — MongoDB (projections)
	MongoDB  string // MONGO_DB — database name (default "eprocurement")
}

// Load reads the configuration from the environment with local-development
// defaults mirroring the compose credentials. Unparseable duration/size
// values fall back to the pinned default rather than failing boot.
func Load() Config {
	return Config{
		Env:            envOr("APP_ENV", "local"),
		LogLevel:       envOr("LOG_LEVEL", "info"),
		DatabaseURL:    envOr("DATABASE_URL", "postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable"),
		PollInterval:   durationOr("WORKER_POLL_INTERVAL", 2*time.Second),
		BatchSize:      intOr("WORKER_BATCH_SIZE", 50),
		ConfirmTimeout: durationOr("WORKER_CONFIRM_TIMEOUT", 5*time.Second),
		RabbitMQURL:    envOr("RABBITMQ_URL", "amqp://eprocurement:eprocurement@localhost:5672/"),
		MongoURI:       envOr("MONGO_URI", "mongodb://eprocurement:eprocurement@localhost:27017/?authSource=admin"),
		MongoDB:        envOr("MONGO_DB", "eprocurement"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

func intOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
