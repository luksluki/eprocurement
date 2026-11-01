// Command worker runs the background jobs in ONE binary under ONE signal
// context (the #45 + #47 + #48 + #49 + #55 composition, dev-notes
// 047/048/049/055):
//
//   - the transactional outbox publisher (#45): poll procurement.outbox,
//     publish the §14.5 envelopes to RabbitMQ, mark published_at;
//   - the Mongo projection consumers (#47): consume documents.history and
//     notifications idempotently (unique event_id dedupe);
//   - the DLQ depth monitor (#48): log an hourly structured summary of
//     procurement.dlq's depth (PRD §16) and publish the depth gauge (#55);
//   - the nightly export scheduler (#49): fire EXPORT_SCHEDULE in EXPORT_TZ,
//     per tenant take the Redis lock, write the PO register CSV to MinIO,
//     keep the run history in Mongo export_runs, detect missed runs;
//   - the export-age refresher (#55): recompute export_age_seconds{tenant}
//     from the Redis heartbeats every EXPORT_AGE_INTERVAL;
//   - the /metrics server (#55): publish the gauges on METRICS_PORT
//     (container-internal; only Prometheus scrapes it).
//
// SIGINT/SIGTERM stops all components gracefully (each Run returns nil); a
// fatal error in any cancels the siblings and fails the process. Boot
// connects to RabbitMQ, MongoDB, Redis, and MinIO with backoff — a
// not-yet-ready dependency never crash-loops the process (PRD §14.8
// "worker down") — and ensures the projection + run-history indexes BEFORE
// consumption starts, so a fresh database dedupes from the first delivery.
// The DLQ monitor does not dial at boot at all: a broker that is down costs
// it one warn per tick. The export scheduler itself is constructed
// fail-fast: an invalid schedule/zone or a lock TTL that does not exceed
// the job budget stops boot rather than silently overlapping (#49).
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
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/internal/platform/metrics"
	"github.com/luksluki/eprocurement/internal/platform/telemetry"
	exportstore "github.com/luksluki/eprocurement/services/documents/export"
	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/notifications"
	"github.com/luksluki/eprocurement/services/documents/objectstore"
	"github.com/luksluki/eprocurement/services/worker/config"
	"github.com/luksluki/eprocurement/services/worker/consumer"
	"github.com/luksluki/eprocurement/services/worker/dlqmonitor"
	"github.com/luksluki/eprocurement/services/worker/export"
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

	// #54: the OTel SDK — spans for the outbox publish, the consumer
	// settles, the pgx queries and redis commands below. Empty endpoint =
	// SDK active, nothing exported.
	tp, err := telemetry.Setup(ctx, "worker", cfg.Env, cfg.OTLPEndpoint)
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}
	defer telemetry.Shutdown(context.Background(), tp, 5*time.Second)

	// Metrics half (#55): the process gauges (outbox lag from the publisher
	// loop, DLQ depth from the monitor, export age from the refresher
	// below) and the /metrics server that publishes them.
	mc := metrics.New()

	// Publisher half (#45): postgres pool (with the #54 pgx query tracer —
	// the outbox Pending/MarkPublished and the export register queries get
	// spans) + broker connection with retry.
	poolCfg, err := telemetry.PGXPoolConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	db, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
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
	loop.SetLagSink(mc.SetOutboxLag) // outbox_lag_seconds (#55)

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
	// broker that is down is one warn per tick, not a boot failure. Since
	// #55 each successful reading also sets the dlq_depth_messages gauge.
	monitor := &dlqmonitor.Monitor{
		URL:      cfg.RabbitMQURL,
		Interval: cfg.DLQSummaryInterval,
		Logger:   log,
		SetDepth: mc.SetDLQDepth,
	}

	// Export half (#49): Redis (lock + heartbeat), MinIO (the single
	// S3_BUCKET documents presigns from), the run-history indexes, then
	// the scheduler — constructed fail-fast on a bad schedule/zone or a
	// TTL that does not exceed the job budget. Since #54 the client also
	// carries the redis span hook (one span per command/pipeline).
	rdb, err := connectRedis(ctx, cfg.RedisURL, log, telemetry.RedisHook{})
	if err != nil {
		if ctx.Err() != nil {
			return nil // shutdown during boot: not an error
		}
		return err
	}
	defer rdb.Close()
	objects, err := objectstore.New(cfg.S3Endpoint, "", cfg.S3AccessKey,
		cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	if err != nil {
		return fmt.Errorf("connect object store: %w", err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("ensure bucket %s: %w", cfg.S3Bucket, err)
	}
	runsStore := exportstore.NewStore(mdb)
	if err := runsStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure export runs indexes: %w", err)
	}
	scheduler, err := export.NewScheduler(export.SchedulerConfig{
		Schedule: cfg.ExportSchedule,
		Timezone: cfg.ExportTZ,
		LockTTL:  cfg.ExportLockTTL,
		Budget:   cfg.ExportJobBudget,
	}, db, objects, export.NewRedisKV(rdb), runsStore, log)
	if err != nil {
		return err
	}

	// Export-age refresher (#55): export_age_seconds{tenant} from the same
	// register + heartbeat KV the scheduler uses.
	ageMonitor := export.NewAgeMonitor(export.NewRegister(db), export.NewRedisKV(rdb),
		mc, cfg.ExportAgeInterval, log)

	log.Info("starting worker",
		"poll_interval", cfg.PollInterval.String(), "batch_size", cfg.BatchSize,
		"dlq_summary_interval", cfg.DLQSummaryInterval.String(),
		"queues", []string{topology.DocumentsHistoryQueue, topology.NotificationsQueue},
		"export_schedule", cfg.ExportSchedule, "export_tz", cfg.ExportTZ,
		"export_age_interval", cfg.ExportAgeInterval.String(), "metrics_port", cfg.MetricsPort)
	return runComponents(ctx, log,
		component{"publisher", loop.Run},
		component{"consumers", runner.Run},
		component{"dlq-monitor", monitor.Run},
		component{"exports", scheduler.Run},
		component{"export-age", ageMonitor.Run},
		component{"metrics", func(ctx context.Context) error {
			return mc.Serve(ctx, ":"+cfg.MetricsPort, log)
		}},
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

// connectRedis dials and pings Redis with exponential backoff (1 s → 10 s
// cap) until the server answers or ctx ends (#49: the export lock and
// heartbeat live there). hooks (may be empty) installs the #54 span hook.
func connectRedis(ctx context.Context, url string, log *slog.Logger, hooks ...redis.Hook) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opts)
	for _, h := range hooks {
		client.AddHook(h)
	}
	backoff := time.Second
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := client.Ping(pingCtx).Err()
		cancel()
		if err == nil {
			return client, nil
		}
		if ctx.Err() == nil {
			log.Warn("redis not ready; retrying", "error", err, "backoff", backoff.String())
		}
		select {
		case <-ctx.Done():
			_ = client.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}
