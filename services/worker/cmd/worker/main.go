// Command worker runs the background jobs. #45 gives it the transactional
// outbox publisher: poll procurement.outbox, publish the §14.5 envelopes to
// RabbitMQ, mark published_at. Consumers, cron, and DLQ retry arrive with
// later issues.
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

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/worker/config"
	"github.com/luksluki/eprocurement/services/worker/publisher"
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
	log.Info("starting worker",
		"poll_interval", cfg.PollInterval.String(), "batch_size", cfg.BatchSize)

	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pub, err := connectWithRetry(ctx, cfg.RabbitMQURL, cfg.ConfirmTimeout, log)
	if err != nil {
		return err
	}
	defer pub.Close()

	loop := publisher.NewLoop(publisher.OutboxDB{DB: db}, pub, log, cfg.PollInterval, cfg.BatchSize)
	log.Info("outbox publisher running")
	return loop.Run(ctx)
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
