// Package config loads worker service configuration from the environment.
//
// The one worker binary runs all four background halves (dev-notes
// 045/047/048/049): the transactional outbox publisher (#45; DATABASE_URL +
// the poll knobs below), the Mongo projection consumers (#47;
// MONGO_URI/MONGO_DB), the DLQ depth monitor (#48; DLQ_SUMMARY_INTERVAL),
// and the nightly export scheduler (#49; REDIS_URL + the EXPORT_* knobs +
// the S3_* object store). RABBITMQ_URL is shared. PollInterval and
// BatchSize are pinned by the PRD (§16: poll every 2 s, batch 50); the env
// overrides exist so tests can tick fast and so operators can tune a demo
// — production keeps the defaults. The export schedule knobs are raw
// strings/durations here; the scheduler constructor validates them
// fail-fast at boot (EXPORT_LOCK_TTL must strictly exceed
// EXPORT_JOB_BUDGET).
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

	// DLQ monitor (#48): hourly depth summary log (PRD §16).
	DLQSummaryInterval time.Duration // DLQ_SUMMARY_INTERVAL (default 1h)

	// Nightly export (#49): Redis lock + heartbeat, schedule, MinIO writer.
	RedisURL        string        // REDIS_URL — distributed lock + heartbeat
	ExportSchedule  string        // EXPORT_SCHEDULE (default "01:00")
	ExportTZ        string        // EXPORT_TZ — IANA zone (default "UTC")
	ExportLockTTL   time.Duration // EXPORT_LOCK_TTL (default 15m; must exceed the budget)
	ExportJobBudget time.Duration // EXPORT_JOB_BUDGET (default 10m)
	S3Endpoint      string        // S3_ENDPOINT — MinIO host:port, no scheme
	S3AccessKey     string        // S3_ACCESS_KEY
	S3SecretKey     string        // S3_SECRET_KEY
	S3Bucket        string        // S3_BUCKET — the single bucket shared with documents
	S3UseSSL        bool          // S3_USE_SSL
}

// Load reads the configuration from the environment with local-development
// defaults mirroring the compose credentials. Unparseable duration/size
// values fall back to the pinned default rather than failing boot.
func Load() Config {
	return Config{
		Env:                envOr("APP_ENV", "local"),
		LogLevel:           envOr("LOG_LEVEL", "info"),
		DatabaseURL:        envOr("DATABASE_URL", "postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable"),
		PollInterval:       durationOr("WORKER_POLL_INTERVAL", 2*time.Second),
		BatchSize:          intOr("WORKER_BATCH_SIZE", 50),
		ConfirmTimeout:     durationOr("WORKER_CONFIRM_TIMEOUT", 5*time.Second),
		RabbitMQURL:        envOr("RABBITMQ_URL", "amqp://eprocurement:eprocurement@localhost:5672/"),
		MongoURI:           envOr("MONGO_URI", "mongodb://eprocurement:eprocurement@localhost:27017/?authSource=admin"),
		MongoDB:            envOr("MONGO_DB", "eprocurement"),
		DLQSummaryInterval: durationOr("DLQ_SUMMARY_INTERVAL", time.Hour),
		RedisURL:           envOr("REDIS_URL", "redis://localhost:6379/0"),
		ExportSchedule:     envOr("EXPORT_SCHEDULE", "01:00"),
		ExportTZ:           envOr("EXPORT_TZ", "UTC"),
		ExportLockTTL:      durationOr("EXPORT_LOCK_TTL", 15*time.Minute),
		ExportJobBudget:    durationOr("EXPORT_JOB_BUDGET", 10*time.Minute),
		S3Endpoint:         envOr("S3_ENDPOINT", "localhost:9000"),
		S3AccessKey:        envOr("S3_ACCESS_KEY", "eprocurement"),
		S3SecretKey:        envOr("S3_SECRET_KEY", "eprocurement"),
		S3Bucket:           envOr("S3_BUCKET", "attachments"),
		S3UseSSL:           envBool("S3_USE_SSL", false),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "1" || v == "true" || v == "yes"
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
