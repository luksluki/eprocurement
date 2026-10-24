// Package publisher drains the procurement transactional outbox to RabbitMQ
// (issue #45; PRD §14.5, §16): poll procurement.outbox every interval in
// batches, assemble the §14.5 envelope from the row columns, publish to the
// durable topic exchange procurement.events with routing key = event_type on
// a confirm-mode channel, and stamp published_at only after the broker
// confirms — at-least-once, with consumers deduping on event_id (§14.8).
//
// The persistence side (outbox.Pending / outbox.MarkPublished) lives with
// the table's owning service under services/procurement/outbox; importing it
// here is a conscious cross-service dependency inside the single Go module
// (documented in docs/schema/procurement.md).
package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/services/procurement/outbox"
)

// Broker topology (PRD §14.5). The events exchange is the publisher's
// contract; the DLX pair is a PROVISIONAL bootstrap declaration — see
// connect.
const (
	EventsExchange = "procurement.events"
	DLXExchange    = "procurement.dlx"
	DLQQueue       = "procurement.dlq"
)

// dialTimeout bounds one AMQP dial attempt.
const dialTimeout = 10 * time.Second

// Envelope is the PRD §14.5 event envelope, assembled 1:1 from an outbox
// row — the columns exist precisely so this needs no payload parsing.
type Envelope struct {
	EventID    string          `json:"event_id"`
	TenantID   string          `json:"tenant_id"`
	Type       string          `json:"type"`
	Version    int32           `json:"version"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorID    string          `json:"actor_id"`
	TraceID    string          `json:"trace_id"`
	Payload    json.RawMessage `json:"payload"`
}

// EnvelopeFromRow maps one unpublished outbox row onto the wire envelope.
// Routing key is the event type (PRD §14.5 pr.* bindings).
func EnvelopeFromRow(r outbox.PendingEvent) Envelope {
	return Envelope{
		EventID: r.ID, TenantID: r.TenantID, Type: r.EventType, Version: r.Version,
		OccurredAt: r.OccurredAt, ActorID: r.ActorID, TraceID: r.TraceID,
		Payload: r.Payload,
	}
}

// Publisher publishes one envelope and returns only after the broker
// confirmed it (or with an error).
type Publisher interface {
	Publish(ctx context.Context, env Envelope) error
}

// OutboxStore is the persistence side of the loop: batched pending reads and
// the published_at marker.
type OutboxStore interface {
	Pending(ctx context.Context, limit int) ([]outbox.PendingEvent, error)
	MarkPublished(ctx context.Context, ids []string) error
}

// OutboxDB adapts the procurement outbox package to OutboxStore.
type OutboxDB struct{ DB *pgxpool.Pool }

func (o OutboxDB) Pending(ctx context.Context, limit int) ([]outbox.PendingEvent, error) {
	return outbox.Pending(ctx, o.DB, limit)
}

func (o OutboxDB) MarkPublished(ctx context.Context, ids []string) error {
	return outbox.MarkPublished(ctx, o.DB, ids)
}

// AMQPPublisher publishes envelopes to the events exchange on a confirm-mode
// channel with persistent delivery. A failed or timed-out publish drops the
// connection; the next call reconnects (one attempt per call), so a broker
// restart costs at most one tick of delay.
type AMQPPublisher struct {
	url            string
	confirmTimeout time.Duration

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

// Connect dials the broker and declares the topology once. It is the boot
// path (main retries it with backoff) and the reconnect path of Publish.
func Connect(ctx context.Context, url string, confirmTimeout time.Duration) (*AMQPPublisher, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("publisher: connect cancelled: %w", err)
	}
	p := &AMQPPublisher{url: url, confirmTimeout: confirmTimeout}
	if err := p.connect(); err != nil {
		return nil, err
	}
	return p, nil
}

// connect dials and declares the channel topology. Not goroutine-safe on its
// own — callers hold the mutex or have exclusive access.
func (p *AMQPPublisher) connect() error {
	conn, err := amqp.DialConfig(p.url, amqp.Config{
		Dial: (&net.Dialer{Timeout: dialTimeout}).Dial,
	})
	if err != nil {
		return fmt.Errorf("publisher: dial rabbitmq: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("publisher: open channel: %w", err)
	}
	if err := ch.ExchangeDeclare(EventsExchange, "topic",
		true, false, false, false, nil); err != nil {
		_ = conn.Close()
		return fmt.Errorf("publisher: declare %s: %w", EventsExchange, err)
	}
	// PROVISIONAL: the full §14.5 dead-lettering model is consumer-side —
	// documents.history / notifications / exports each declare
	// x-dead-letter-exchange with a TTL-backoff retry queue (5s → 30s → 5m),
	// arriving with the consumer issues. This one idempotent bootstrap
	// declaration (durable DLX + a # bound DLQ) exists so a future
	// dead-letter has a destination and is admin-visible from day one; it is
	// expected to be reshaped, not final.
	if err := ch.ExchangeDeclare(DLXExchange, "topic",
		true, false, false, false, nil); err != nil {
		_ = conn.Close()
		return fmt.Errorf("publisher: declare %s: %w", DLXExchange, err)
	}
	if _, err := ch.QueueDeclare(DLQQueue,
		true, false, false, false, nil); err != nil {
		_ = conn.Close()
		return fmt.Errorf("publisher: declare %s: %w", DLQQueue, err)
	}
	if err := ch.QueueBind(DLQQueue, "#", DLXExchange, false, nil); err != nil {
		_ = conn.Close()
		return fmt.Errorf("publisher: bind %s: %w", DLQQueue, err)
	}
	if err := ch.Confirm(false); err != nil {
		_ = conn.Close()
		return fmt.Errorf("publisher: confirm mode: %w", err)
	}
	p.conn, p.ch = conn, ch
	return nil
}

// drop tears the connection down so the next Publish reconnects clean.
func (p *AMQPPublisher) drop() {
	if p.conn != nil {
		_ = p.conn.Close() // closes the channel with it
	}
	p.conn, p.ch = nil, nil
}

// Close releases the connection.
func (p *AMQPPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return nil
	}
	err := p.conn.Close()
	p.drop()
	return err
}

// Publish marshals the envelope, publishes it to the events exchange with
// routing key = event type and persistent delivery, and waits for the broker
// confirm before returning. A confirm that never arrives inside
// confirmTimeout, an explicit nack, or a transport error returns an error —
// the caller keeps the row's published_at NULL and retries next tick
// (at-least-once; consumers dedupe on event_id, PRD §14.8).
func (p *AMQPPublisher) Publish(ctx context.Context, env Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("publisher: marshal envelope %s: %w", env.EventID, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil || p.conn.IsClosed() {
		if err := p.connect(); err != nil {
			return err
		}
	}
	dc, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, EventsExchange, env.Type,
		false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    env.EventID,
			Timestamp:    env.OccurredAt,
			Body:         body,
		})
	if err != nil {
		p.drop()
		return fmt.Errorf("publisher: publish %s: %w", env.EventID, err)
	}
	cctx, cancel := context.WithTimeout(ctx, p.confirmTimeout)
	defer cancel()
	acked, err := dc.WaitContext(cctx)
	if err != nil {
		// Timeout or shutdown while a confirm was in flight: the channel's
		// confirm state is unknown, so reset the connection.
		p.drop()
		return fmt.Errorf("publisher: confirm wait %s: %w", env.EventID, err)
	}
	if !acked {
		// Explicit nack: the broker refused the message. Keep the channel
		// (it is still healthy) and let the next tick retry.
		return fmt.Errorf("publisher: broker nacked %s", env.EventID)
	}
	return nil
}

// Loop is the outbox publisher loop (PRD §16): poll → publish → mark,
// forever. Every failure is non-fatal by design — a fresh compose volume
// starts the worker before the procurement service has applied migration
// 0006 (the worker deliberately runs no migrations; procurement is the only
// applier), so the first polls can hit a missing schema, and a broker or
// database restart mid-run looks the same. The loop logs and waits for the
// next tick instead of dying — a dead loop container would fail the compose
// gate and the resilience demo (§14.8 "worker down" row).
type Loop struct {
	store     OutboxStore
	pub       Publisher
	log       *slog.Logger
	interval  time.Duration
	batchSize int
}

// NewLoop builds the loop. interval and batchSize come from config (pinned
// 2 s / 50; env overrides exist for tests).
func NewLoop(store OutboxStore, pub Publisher, log *slog.Logger, interval time.Duration, batchSize int) *Loop {
	return &Loop{store: store, pub: pub, log: log, interval: interval, batchSize: batchSize}
}

// Run polls once immediately, then every interval, until ctx is cancelled
// (SIGINT/SIGTERM) — then returns nil.
func (l *Loop) Run(ctx context.Context) error {
	l.pollOnce(ctx)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			l.pollOnce(ctx)
		}
	}
}

// pollOnce drains one batch: poll, publish each row, mark the confirmed
// prefix. Failures log a warn and end the tick — the loop itself never
// exits on them.
func (l *Loop) pollOnce(ctx context.Context) {
	rows, err := l.store.Pending(ctx, l.batchSize)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not a publish-path failure
		}
		l.log.Warn("outbox poll failed", "error", err)
		return
	}
	published := make([]string, 0, len(rows))
	for _, r := range rows {
		env := EnvelopeFromRow(r)
		if err := l.pub.Publish(ctx, env); err != nil {
			if ctx.Err() != nil {
				return // shutting down mid-batch; rows retry next run
			}
			// Broker down or confirm timeout. The row keeps published_at
			// NULL and reappears next tick; stop the batch here to keep
			// ordering and let the tail retry after the failure.
			l.log.Warn("outbox publish failed",
				"event_id", r.ID, "tenant_id", r.TenantID,
				"event_type", r.EventType, "trace_id", r.TraceID, "error", err)
			break
		}
		published = append(published, r.ID)
		l.log.Info("outbox event published",
			"event_id", r.ID, "tenant_id", r.TenantID,
			"event_type", r.EventType, "trace_id", r.TraceID)
	}
	if len(published) == 0 {
		return
	}
	if err := l.store.MarkPublished(ctx, published); err != nil {
		if ctx.Err() != nil {
			return
		}
		// The confirmed events are on the broker; the unmarked rows are
		// re-published next tick and deduped by consumers.
		l.log.Warn("outbox mark published failed", "count", len(published), "error", err)
	}
}
