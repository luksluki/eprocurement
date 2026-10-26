// Command worker runs the background jobs in ONE binary under ONE signal
// context (the #45 + #47 composition, dev-note 047):
//
//   - the transactional outbox publisher (#45): poll procurement.outbox,
//     publish the §14.5 envelopes to RabbitMQ, mark published_at;
//   - the Mongo projection consumers (#47): consume documents.history and
//     notifications idempotently (unique event_id dedupe).
//
// SIGINT/SIGTERM stops both halves gracefully (each Run returns nil); a
// fatal error in either cancels the sibling and fails the process. Boot
// connects to RabbitMQ and MongoDB with backoff — a not-yet-ready
// dependency never crash-loops the process (PRD §14.8 "worker down") — and
// ensures the projection indexes BEFORE consumption starts, so a fresh
// database dedupes from the first delivery.
//
// The worker deliberately runs no database migrations: procurement is the
// only migration applier (single-writer, no advisory-lock races — dev-note
// 030), so on a fresh volume the worker's first polls may hit a missing
// schema; the publisher loop treats poll errors as non-fatal and recovers
// once procurement has applied them.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/notifications"
	"github.com/luksluki/eprocurement/services/worker/config"
	"github.com/luksluki/eprocurement/services/worker/consumer"
	"github.com/luksluki/eprocurement/services/worker/publisher"
	"github.com/luksluki/eprocurement/services/worker/topology"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Publisher half (#45): postgres pool + broker connection with retry.
	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer db.Close()
	pub, err := connectWithRetry(ctx, cfg.RabbitMQURL, cfg.ConfirmTimeout, log)
	if err != nil {
		if ctx.Err() != nil {
			return nil // shutdown during boot: not an error
		}
		return err
	}
	defer pub.Close()
	loop := publisher.NewLoop(publisher.OutboxDB{DB: db}, pub, log, cfg.PollInterval, cfg.BatchSize)

	// Consumer half (#47): Mongo connect+ping with backoff, then the
	// projection indexes BEFORE consumption starts — a fresh database
	// dedupes from the first delivery (PRD §14.5 idempotency key =
	// event_id, unique index).
	client, err := connectMongo(ctx, cfg.MongoURI, log)
	if err != nil {
		if ctx.Err() != nil {
			return nil // shutdown during boot: not an error
		}
		return err
	}
	defer client.Disconnect(context.Background())
	mdb := client.Database(cfg.MongoDB)
	histStore := history.NewStore(mdb)
	if err := histStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure history indexes: %w", err)
	}
	notifStore := notifications.NewStore(mdb)
	if err := notifStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure notifications indexes: %w", err)
	}
	runner, err := consumer.New(consumer.Config{
		URL: cfg.RabbitMQURL,
		Handlers: map[string]consumer.Handler{
			topology.DocumentsHistoryQueue: histStore,
			topology.NotificationsQueue:    notifStore,
		},
		Logger: log,
	})
	if err != nil {
		return err
	}

	log.Info("starting worker",
		"poll_interval", cfg.PollInterval.String(), "batch_size", cfg.BatchSize,
		"queues", []string{topology.DocumentsHistoryQueue, topology.NotificationsQueue})
	return runBoth(ctx, log, loop, runner)
}

// runBoth runs the publisher loop and the consumer runner concurrently
// under one derived context. Both Run methods return nil when the context
// is cancelled (SIGINT/SIGTERM): each settles its in-flight work and stops.
// A non-nil error from either half is fatal for the process: it cancels the
// sibling (so the OTHER half stops too instead of running on alone) and is
// returned after both have exited.
func runBoth(ctx context.Context, log *slog.Logger, loop *publisher.Loop, runner *consumer.Runner) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type component struct {
		name string
		err  error
	}
	results := make(chan component, 2)
	go func() { results <- component{"publisher", loop.Run(runCtx)} }()
	go func() { results <- component{"consumers", runner.Run(runCtx)} }()

	var firstErr error
	for i := 0; i < 2; i++ {
		c := <-results
		if c.err == nil {
			log.Info("worker component stopped", "component", c.name)
			continue
		}
		log.Error("worker component failed", "component", c.name, "error", c.err)
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", c.name, c.err)
			cancel() // stop the sibling half as well
		}
	}
	return firstErr
}

// connectWithRetry dials RabbitMQ with exponential backoff (1s doubling to a
// 10s cap) until it connects or ctx is done — boot stays alive through a
// broker that is still starting instead of crash-looping the container.
func connectWithRetry(ctx context.Context, url string, confirmTimeout time.Duration, log *slog.Logger) (*publisher.AMQPPublisher, error) {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		pub, err := publisher.Connect(ctx, url, confirmTimeout)
		if err == nil {
			if attempt > 1 {
				log.Info("rabbitmq connected", "attempts", attempt)
			}
			return pub, nil
		}
		log.Warn("rabbitmq connect failed; retrying",
			"attempt", attempt, "sleep", delay.String(), "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 10*time.Second {
			delay *= 2
		}
	}
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
