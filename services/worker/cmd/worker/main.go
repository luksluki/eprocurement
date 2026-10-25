// Command worker runs the background consumers (issue #47): connect to
// MongoDB and RabbitMQ with backoff (a not-yet-ready dependency never
// crash-loops the process — PRD §14.8 "worker down"), ensure the projection
// indexes BEFORE consumption starts, then consume documents.history and
// notifications idempotently until SIGINT/SIGTERM.
//
// NOTE (#47): PR #94 (#45 publisher) rewrites this file for the outbox
// publisher runner; whichever PR merges last composes the two under one
// signal context — publisher Loop + these consumers in one binary
// (dev-note 047).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/notifications"
	"github.com/luksluki/eprocurement/services/worker/config"
	"github.com/luksluki/eprocurement/services/worker/consumer"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	log := logger.New("worker", cfg.Env, cfg.LogLevel)
	log.Info("starting worker consumers",
		"queues", []string{consumer.DocumentsHistoryQueue, consumer.NotificationsQueue})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Mongo connect+ping with backoff until ready or shutdown — boot
	// tolerates a slow-starting Mongo instead of crash-looping.
	client, err := connectMongo(ctx, cfg.MongoURI, log)
	if err != nil {
		if ctx.Err() != nil {
			return nil // shutdown during boot: not an error
		}
		return err
	}
	defer client.Disconnect(context.Background())
	db := client.Database(cfg.MongoDB)

	// Indexes before consumption: a fresh database dedupes from the first
	// delivery (PRD §14.5 idempotency key = event_id, unique index).
	histStore := history.NewStore(db)
	if err := histStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure history indexes: %w", err)
	}
	notifStore := notifications.NewStore(db)
	if err := notifStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure notifications indexes: %w", err)
	}

	runner, err := consumer.New(consumer.Config{
		URL: cfg.RabbitMQURL,
		Handlers: map[string]consumer.Handler{
			consumer.DocumentsHistoryQueue: histStore,
			consumer.NotificationsQueue:    notifStore,
		},
		Logger: log,
	})
	if err != nil {
		return err
	}
	return runner.Run(ctx)
}

// connectMongo dials and pings MongoDB with exponential backoff (1 s → 10 s
// cap) until the server answers or ctx ends.
func connectMongo(ctx context.Context, uri string, log *slog.Logger) (*mongo.Client, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("connect mongo: %w", err)
	}
	backoff := time.Second
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := client.Ping(pingCtx, nil)
		cancel()
		if err == nil {
			return client, nil
		}
		if ctx.Err() == nil {
			log.Warn("mongo not ready; retrying", "error", err, "backoff", backoff.String())
		}
		select {
		case <-ctx.Done():
			_ = client.Disconnect(context.Background())
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}
