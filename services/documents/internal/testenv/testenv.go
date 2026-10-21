// Package testenv provides MongoDB and MinIO test environments for the
// documents service integration tests. Each helper skips the test when the
// corresponding TEST_* environment variable is unset. The helpers return
// primitives only so no package gains a test-only import cycle.
package testenv

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Mongo connects to TEST_MONGO_URI and returns a uniquely-named database
// that is dropped when the test finishes.
func Mongo(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("TEST_MONGO_URI")
	if uri == "" {
		t.Skip("TEST_MONGO_URI not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("ping mongo: %v", err)
	}
	dbName := fmt.Sprintf("documents_test_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = client.Database(dbName).Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	return client.Database(dbName)
}

// MinIOEnv returns TEST_S3_ENDPOINT / TEST_S3_ACCESS_KEY / TEST_S3_SECRET_KEY
// (with local defaults), skipping the test when the endpoint is unset. The
// caller builds its own objectstore client with these parameters.
func MinIOEnv(t *testing.T) (endpoint, accessKey, secretKey, bucket string) {
	t.Helper()
	endpoint = os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set; skipping integration test")
	}
	return endpoint, envOr("TEST_S3_ACCESS_KEY", "eprocurement"),
		envOr("TEST_S3_SECRET_KEY", "eprocurement"), "attachments-test"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
