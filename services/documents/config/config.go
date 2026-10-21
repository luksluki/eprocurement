// Package config loads documents service configuration from the environment.
package config

import (
	"os"
	"time"
)

// Config holds the documents service runtime configuration.
type Config struct {
	Env      string // APP_ENV (default "local")
	Port     string // PORT — gRPC listen port (default "8083")
	LogLevel string // LOG_LEVEL (default "info")

	MongoURI string // MONGO_URI — MongoDB connection string (document_meta)
	MongoDB  string // MONGO_DB — database name (default "eprocurement")

	S3Endpoint        string // S3_ENDPOINT — MinIO host:port, no scheme (default "localhost:9000")
	S3PresignEndpoint string // S3_PRESIGN_ENDPOINT — host minted into presigned URLs; empty = S3_ENDPOINT
	S3AccessKey       string // S3_ACCESS_KEY
	S3SecretKey       string // S3_SECRET_KEY
	S3Bucket          string // S3_BUCKET (default "attachments")
	S3UseSSL          bool   // S3_USE_SSL (default false)

	AttachmentMaxSizeMB int           // ATTACHMENT_MAX_SIZE_MB (default 25)
	PendingTTL          time.Duration // ATTACHMENT_PENDING_TTL — reaper threshold (default 24h)
	ReaperInterval      time.Duration // REAPER_INTERVAL — 0 or less disables the job (default 30m)
}

// Load reads the configuration from the environment with local-development
// defaults.
func Load() Config {
	return Config{
		Env:                 envOr("APP_ENV", "local"),
		Port:                envOr("PORT", "8083"),
		LogLevel:            envOr("LOG_LEVEL", "info"),
		MongoURI:            envOr("MONGO_URI", "mongodb://eprocurement:eprocurement@localhost:27017/?authSource=admin"),
		MongoDB:             envOr("MONGO_DB", "eprocurement"),
		S3Endpoint:          envOr("S3_ENDPOINT", "localhost:9000"),
		S3PresignEndpoint:   envOr("S3_PRESIGN_ENDPOINT", ""),
		S3AccessKey:         envOr("S3_ACCESS_KEY", "eprocurement"),
		S3SecretKey:         envOr("S3_SECRET_KEY", "eprocurement"),
		S3Bucket:            envOr("S3_BUCKET", "attachments"),
		S3UseSSL:            envBool("S3_USE_SSL", false),
		AttachmentMaxSizeMB: envInt("ATTACHMENT_MAX_SIZE_MB", 25),
		PendingTTL:          envDuration("ATTACHMENT_PENDING_TTL", 24*time.Hour),
		ReaperInterval:      envDuration("REAPER_INTERVAL", 30*time.Minute),
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

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				return fallback
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
