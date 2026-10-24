// Command documents runs the documents service: it ensures the MinIO
// bucket and the MongoDB indexes (attachment metadata + the append-only
// event history), starts the orphan-upload reaper, then serves gRPC
// (documents.v1 + health + reflection) until SIGINT/SIGTERM.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/documents/attachment"
	documentsconfig "github.com/luksluki/eprocurement/services/documents/config"
	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/objectstore"
	"github.com/luksluki/eprocurement/services/documents/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "documents: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := documentsconfig.Load()
	log := logger.New("documents", cfg.Env, cfg.LogLevel)
	log.Info("starting documents service", "port", cfg.Port)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		return fmt.Errorf("connect mongo: %w", err)
	}
	defer mongoClient.Disconnect(context.Background())
	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	defer cancelPing()
	if err := mongoClient.Ping(pingCtx, nil); err != nil {
		return fmt.Errorf("ping mongo: %w", err)
	}
	db := mongoClient.Database(cfg.MongoDB)
	store := attachment.NewStore(db)
	if err := store.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure indexes: %w", err)
	}
	events := history.NewStore(db)
	if err := events.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure history indexes: %w", err)
	}

	storage, err := objectstore.New(cfg.S3Endpoint, cfg.S3PresignEndpoint,
		cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	if err != nil {
		return fmt.Errorf("connect object store: %w", err)
	}
	if err := storage.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("ensure bucket %s: %w", cfg.S3Bucket, err)
	}

	if cfg.ReaperInterval > 0 {
		reaper := attachment.NewReaper(store, storage, cfg.PendingTTL, log)
		go reaper.Loop(ctx, cfg.ReaperInterval)
	} else {
		log.Info("attachment reaper disabled (REAPER_INTERVAL <= 0)")
	}

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Port, err)
	}
	srv := server.New(log, pinger{client: mongoClient}, store, storage,
		attachment.Limits{MaxSizeBytes: int64(cfg.AttachmentMaxSizeMB) << 20}, events)
	return srv.Serve(ctx, lis)
}

// pinger adapts the MongoDB client to the gRPC health-check Pinger.
type pinger struct {
	client *mongo.Client
}

func (p pinger) Ping(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.client.Ping(pingCtx, nil)
}
