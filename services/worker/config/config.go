// Package config loads worker service configuration from the environment.
//
// NOTE (#47): PR #94 (#45 publisher) carries a sibling services/worker/
// config with DATABASE_URL + poll knobs; whichever PR merges last folds
// the two into one Config — the fields do not overlap (dev-note 047).
package config

import (
	"os"
)

// Config holds the worker consumer runtime configuration.
type Config struct {
	Env         string // APP_ENV (default "local")
	LogLevel    string // LOG_LEVEL (default "info")
	RabbitMQURL string // RABBITMQ_URL — AMQP broker
	MongoURI    string // MONGO_URI — MongoDB (projections)
	MongoDB     string // MONGO_DB — database name (default "eprocurement")
}

// Load reads the configuration from the environment with local-development
// defaults mirroring the documents service (MONGO_URI) and the compose
// credentials (RABBITMQ_URL).
func Load() Config {
	return Config{
		Env:         envOr("APP_ENV", "local"),
		LogLevel:    envOr("LOG_LEVEL", "info"),
		RabbitMQURL: envOr("RABBITMQ_URL", "amqp://eprocurement:eprocurement@localhost:5672/"),
		MongoURI:    envOr("MONGO_URI", "mongodb://eprocurement:eprocurement@localhost:27017/?authSource=admin"),
		MongoDB:     envOr("MONGO_DB", "eprocurement"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
