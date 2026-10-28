// Command worker runs the background jobs in ONE binary under ONE signal
// context (the #45 + #47 + #48 composition, dev-notes 047/048):
//
//   - the transactional outbox publisher (#45): poll procurement.outbox,
//     publish the §14.5 envelopes to RabbitMQ, mark published_at;
//   - the Mongo projection consumers (#47): consume documents.history and
//     notifications idempotently (unique event_id dedupe);
//   - the DLQ depth monitor (#48): log an hourly structured summary of
//     procurement.dlq's depth (PRD §16).
//
// SIGINT/SIGTERM stops all components gracefully (each Run returns nil); a
// fatal error in any cancels the siblings and fails the process. Boot
// connects to RabbitMQ and MongoDB with backoff — a not-yet-ready
// dependency never crash-loops the process (PRD §14.8 "worker down") — and
// ensures the projection indexes BEFORE consumption starts, so a fresh
// database dedupes from the first delivery. The DLQ monitor does not dial
// at boot at all: a broker that is down costs it one warn per tick.
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
	"github.com/luksluki/eprocurement/services/worker/dlqmonitor"
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

	// DLQ monitor half (#48): hourly depth summary, no boot dial — a
	// broker that is down is one warn per tick, not a boot failure.
	monitor := &dlqmonitor.Monitor{
		URL:      cfg.RabbitMQURL,
		Interval: cfg.DLQSummaryInterval,
		Logger:   log,
	}

	log.Info("starting worker",
		"poll_interval", cfg.PollInterval.String(), "batch_size", cfg.BatchSize,
		"dlq_summary_interval", cfg.DLQSummaryInterval.String(),
		"queues", []string{topology.DocumentsHistoryQueue, topology.NotificationsQueue})
	return runComponents(ctx, log,
		component{"publisher", loop.Run},
		component{"consumers", runner.Run},
		component{"dlq-monitor", monitor.Run},
	)
}

// component is one named Run(ctx) task of the worker binary.
type component struct {
	name string
	run  func(ctx context.Context) error
}

// runComponents runs the components concurrently under one derived
// context (runBoth of dev-note 047, generalized from two to N). Every Run
// returns nil when the context is cancelled (SIGINT/SIGTERM): each settles
// its in-flight work and stops. A non-nil error from any component is
// fatal for the process: it cancels the siblings (so the others stop too
// instead of running on alone) and is returned after all have exited.
func runComponents(ctx context.Context, log *slog.Logger, comps ...component) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(comps))
	for _, c := range comps {
		go func(c component) { results <- result{c.name, c.run(runCtx)} }(c)
	}

	var firstErr error
	for range comps {
		r := <-results
		if r.err == nil {
			log.Info("worker component stopped", "component", r.name)
			continue
		}
		log.Error("worker component failed", "component", r.name, "error", r.err)
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", r.name, r.err)
			cancel() // stop the sibling components as well
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
