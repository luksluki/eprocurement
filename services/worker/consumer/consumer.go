// Package consumer runs the worker's RabbitMQ consumers (issue #47; PRD
// §14.5, §14.8, ADR-0001): documents.history projects every domain event
// into the Mongo event store (#42) and notifications writes the
// notifications projection (#47) — both idempotently, deduped by the unique
// event_id index, so at-least-once delivery has exactly one durable effect
// per event. The broker topology (exchanges, queues, bindings, retry ladder)
// is the shared services/worker/topology package (#46); the wire envelope is
// the shared services/worker/publisher Envelope (#45) — this package is only
// the consume/settle loop.
//
// Delivery outcomes (each logged with event_id/tenant_id/type/queue/
// trace_id):
//
//	applied    — the projection inserted a new document; ack.
//	duplicate  — the projection already had the event; ack (PRD §14.8
//	             "duplicate delivery" row).
//	retry-tier — transient failure; a copy is published to the next TTL
//	             retry queue via the default exchange with x-retry-count
//	             raised, then the original is acked.
//	dlq        — permanent failure (undecodable / invalid envelope) or
//	             exhausted retries; nack without requeue, the queue's
//	             x-dead-letter-exchange routes it to procurement.dlq.
//
// Run returns nil when ctx is cancelled (SIGINT/SIGTERM): in-flight
// handlers finish, the connection closes, and unacknowledged deliveries
// return to their queue — redelivered after restart, deduped by the store.
// A lost connection (broker restart) is logged and re-dialed with backoff;
// boot behaves the same, so a not-yet-ready broker never crash-loops the
// worker (PRD §14.8 "worker down" row).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/worker/publisher"
	"github.com/luksluki/eprocurement/services/worker/topology"
)

// dialTimeout bounds one AMQP dial attempt.
const dialTimeout = 10 * time.Second

// Reconnect backoff bounds (1 s → 10 s, the publisher's #45 convention).
const (
	initialBackoff = time.Second
	maxBackoff     = 10 * time.Second
)

// ErrEnvelopeInvalid is the typed error for deliveries that can never
// succeed: a body that does not decode, or an envelope that fails
// validation. The runner dead-letters those straight to the DLQ without
// retry (they would fail forever — PRD §14.5 "after max attempts → DLQ"
// shortened to zero attempts for permanent failures).
var ErrEnvelopeInvalid = errors.New("consumer_envelope_invalid")

// Decode parses one delivery body into the shared publisher.Envelope (the
// §14.5 wire format both halves of the worker speak). Any decode failure is
// ErrEnvelopeInvalid — the message is permanently broken, not transient.
func Decode(body []byte) (publisher.Envelope, error) {
	var env publisher.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return publisher.Envelope{}, fmt.Errorf("%w: decode body: %v", ErrEnvelopeInvalid, err)
	}
	return env, nil
}

// Event validates the envelope and maps it onto the history projection's
// Event (the shape both stores' Apply take). history.NewEvent normalizes
// (trims, defaults the version) and validates in one step, so the same
// checks the stores re-apply defensively decide the delivery's fate here.
// It was a method of the pre-merge inline Envelope type; with the shared
// publisher.Envelope it is a plain helper (methods cannot attach across
// packages).
func Event(env publisher.Envelope) (history.Event, error) {
	ev, err := history.NewEvent(env.EventID, env.TenantID, env.Type, env.OccurredAt,
		env.ActorID, env.TraceID, env.EntityType, env.EntityID, string(env.Payload))
	if err != nil {
		return history.Event{}, fmt.Errorf("%w: %v", ErrEnvelopeInvalid, err)
	}
	return ev, nil
}

// isPermanent reports whether a handler error can never succeed on retry —
// the history validation sentinels (both stores validate through them).
// Everything else (Mongo down, network blip) is transient and takes the
// retry ladder.
func isPermanent(err error) bool {
	return errors.Is(err, history.ErrEventIDInvalid) ||
		errors.Is(err, history.ErrEventIncomplete) ||
		errors.Is(err, history.ErrPayloadInvalid)
}

// Handler projects one event. Both history.Store and notifications.Store
// satisfy it (Apply is the idempotency boundary, PRD §14.5).
type Handler interface {
	Apply(ctx context.Context, ev history.Event) (applied bool, err error)
}

// Config parameterizes the runner. URL, Handlers, and Logger are required;
// New fills the defaults for the rest and validates the whole set.
type Config struct {
	URL      string
	Topology topology.Topology  // zero value → topology.Default
	Handlers map[string]Handler // queue name → projection
	Logger   *slog.Logger

	// Prefetch bounds unacknowledged deliveries per session (Qos).
	Prefetch int // default 10
	// DeliveryTimeout bounds one handler call, so a hung store call cannot
	// stall a queue forever — the timeout error is transient and retries.
	DeliveryTimeout time.Duration // default 10 s
}

// Runner consumes the configured queues until ctx is cancelled.
type Runner struct {
	url             string
	topo            topology.Topology
	handlers        map[string]Handler
	log             *slog.Logger
	prefetch        int
	deliveryTimeout time.Duration
}

// New validates the configuration and returns the runner. Every handler
// key must name a queue of the topology — a handler without its queue (or
// vice versa) is a wiring bug that would silently consume nothing. Queues
// without a handler (e.g. exports until its own issue) are declared but not
// consumed.
func New(cfg Config) (*Runner, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("consumer: url is required")
	}
	if len(cfg.Handlers) == 0 {
		return nil, fmt.Errorf("consumer: at least one handler is required")
	}
	topo := cfg.Topology
	if topo.EventsExchange == "" && topo.DLXExchange == "" && topo.DLQQueue == "" && len(topo.Queues) == 0 {
		topo = topology.Default
	}
	if len(topo.Queues) == 0 {
		return nil, fmt.Errorf("consumer: topology declares no queues")
	}
	declared := map[string]bool{}
	for _, q := range topo.Queues {
		declared[q.Name] = true
	}
	for queue := range cfg.Handlers {
		if !declared[queue] {
			return nil, fmt.Errorf("consumer: handler queue %q is not declared by the topology", queue)
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	prefetch := cfg.Prefetch
	if prefetch <= 0 {
		prefetch = 10
	}
	timeout := cfg.DeliveryTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Runner{
		url: cfg.URL, topo: topo, handlers: cfg.Handlers, log: log,
		prefetch: prefetch, deliveryTimeout: timeout,
	}, nil
}

// Run consumes until ctx is cancelled (returns nil) — reconnecting with
// backoff whenever the broker session drops.
func (r *Runner) Run(ctx context.Context) error {
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		started := time.Now()
		err := r.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		r.log.Warn("consumer session ended; reconnecting", "error", err, "backoff", backoff.String())
		// A session that outlived the max backoff was healthy: reset.
		if time.Since(started) > maxBackoff {
			backoff = initialBackoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// taggedDelivery couples a delivery with the queue it arrived on (routing
// alone cannot tell: retry returns carry the main queue's name, and one
// channel serves several queues).
type taggedDelivery struct {
	queue string
	d     amqp.Delivery
}

// session dials, declares the topology, and dispatches deliveries until the
// context is cancelled (nil) or the connection is lost (error).
func (r *Runner) session(ctx context.Context) error {
	conn, err := amqp.DialConfig(r.url, amqp.Config{
		Dial: (&net.Dialer{Timeout: dialTimeout}).Dial,
	})
	if err != nil {
		return fmt.Errorf("consumer: dial rabbitmq: %w", err)
	}
	defer func() { _ = conn.Close() }() // releases unacked deliveries back to the queues
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("consumer: open channel: %w", err)
	}
	if err := r.topo.Declare(ch); err != nil {
		return err
	}
	if err := ch.Qos(r.prefetch, 0, false); err != nil {
		return fmt.Errorf("consumer: set qos: %w", err)
	}

	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	merged := make(chan taggedDelivery)
	var forwarders sync.WaitGroup
	for queue := range r.handlers {
		deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
		if err != nil {
			return fmt.Errorf("consumer: consume %s: %w", queue, err)
		}
		forwarders.Add(1)
		go func(queue string, in <-chan amqp.Delivery) {
			defer forwarders.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case d, ok := <-in:
					if !ok {
						return
					}
					select {
					case merged <- taggedDelivery{queue: queue, d: d}:
					case <-ctx.Done():
						return
					}
				}
			}
		}(queue, deliveries)
	}
	go func() { forwarders.Wait(); close(merged) }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-closed:
			if !ok || err == nil {
				err = amqp.ErrClosed
			}
			return fmt.Errorf("consumer: connection lost: %w", err)
		case td, ok := <-merged:
			if !ok { // every consume channel closed: the connection is gone
				return fmt.Errorf("consumer: deliveries closed: %w", amqp.ErrClosed)
			}
			r.handle(ctx, ch, td)
		}
	}
}

// handle settles one delivery: decode → validate → project → ack / retry /
// dead-letter. Errors from Ack/Nack are deliberately ignored: after a lost
// connection they always fail, and the broker then redelivers — which the
// stores dedupe (PRD §14.8).
func (r *Runner) handle(ctx context.Context, ch *amqp.Channel, td taggedDelivery) {
	queue, d := td.queue, td.d
	log := r.log.With("queue", queue)

	env, err := Decode(d.Body)
	if err != nil {
		_ = d.Nack(false, false) // permanent: straight to the DLQ
		log.Warn("delivery dead-lettered", "status", "dlq", "reason", "decode_failed", "error", err)
		return
	}
	log = log.With("event_id", env.EventID, "tenant_id", env.TenantID,
		"type", env.Type, "trace_id", env.TraceID)

	ev, err := Event(env)
	if err != nil {
		_ = d.Nack(false, false) // permanent (e.g. missing entity_type)
		log.Warn("delivery dead-lettered", "status", "dlq", "reason", "invalid_envelope", "error", err)
		return
	}

	hctx, cancel := context.WithTimeout(ctx, r.deliveryTimeout)
	applied, err := r.handlers[queue].Apply(hctx, ev)
	cancel()
	if err == nil {
		_ = d.Ack(false)
		status := "applied"
		if !applied {
			status = "duplicate"
		}
		log.Info("delivery settled", "status", status)
		return
	}
	if isPermanent(err) {
		_ = d.Nack(false, false) // will never succeed: no retry
		log.Warn("delivery dead-lettered", "status", "dlq", "reason", "invalid_event", "error", err)
		return
	}

	// Transient failure: climb the TTL retry ladder; when it is exhausted,
	// nack without requeue so the broker dead-letters to the DLQ.
	retryCount := topology.RetryCount(d.Headers)
	target, ok := topology.NextRetry(queue, retryCount)
	if !ok {
		_ = d.Nack(false, false)
		log.Warn("delivery dead-lettered", "status", "dlq",
			"reason", "max_retries", "retry_count", retryCount, "error", err)
		return
	}
	if err := publishRetry(ctx, ch, target, d, retryCount+1); err != nil {
		// Could not park the retry copy: requeue the original so nothing is
		// lost — it comes straight back and climbs the ladder next time.
		_ = d.Nack(false, true)
		log.Warn("delivery requeued", "status", "requeue",
			"reason", "retry_publish_failed", "error", err)
		return
	}
	_ = d.Ack(false)
	log.Warn("delivery scheduled for retry", "status", "retry-tier",
		"retry_queue", target, "retry_count", retryCount+1, "error", err)
}

// publishRetry parks a copy of the delivery on the retry-tier queue via the
// default exchange (routing key = queue name) with the retry count raised.
func publishRetry(ctx context.Context, ch *amqp.Channel, target string, d amqp.Delivery, count int32) error {
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[topology.RetryCountHeader] = count
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ch.PublishWithContext(pctx, "", target, false, false, amqp.Publishing{
		Headers:      headers,
		ContentType:  d.ContentType,
		Type:         d.Type,
		MessageId:    d.MessageId,
		DeliveryMode: amqp.Persistent,
		Body:         d.Body,
	})
}
