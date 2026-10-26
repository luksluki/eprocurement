package consumer

// Integration tests against a real RabbitMQ (TEST_RABBITMQ_URL) and a real
// MongoDB (TEST_MONGO_URI) — both skip cleanly when unset (CI has neither
// broker service and is not changed). Every test uses its own suffixed
// topology with 150 ms retry TTLs and a fresh scratch database whose
// indexes are ensured through the same boot path as cmd/worker
// (history + notifications EnsureIndexes before consumption starts), which
// is what proves the unique event_id dedupe works on a brand-new database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/notifications"
	"github.com/luksluki/eprocurement/services/worker/publisher"
	"github.com/luksluki/eprocurement/services/worker/topology"
)

const (
	itPoll     = 50 * time.Millisecond
	itDeadline = 15 * time.Second
)

type itEnv struct {
	amqpURL  string
	mongoDB  *mongo.Database
	topo     topology.Topology
	testConn *amqp.Connection
	hist     string // suffixed main queue names
	notif    string
}

func newItEnv(t *testing.T) *itEnv {
	t.Helper()
	amqpURL := os.Getenv("TEST_RABBITMQ_URL")
	mongoURI := os.Getenv("TEST_MONGO_URI")
	if amqpURL == "" || mongoURI == "" {
		t.Skip("TEST_RABBITMQ_URL/TEST_MONGO_URI not set; skipping integration test")
	}

	// Fresh scratch database + the boot path's index ensure (cmd/worker:
	// both stores' EnsureIndexes run before consumption starts).
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("ping mongo: %v", err)
	}
	dbName := fmt.Sprintf("worker_consumer_it_%d", time.Now().UnixNano())
	db := client.Database(dbName)
	t.Cleanup(func() {
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	if err := history.NewStore(db).EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("ensure history indexes: %v", err)
	}
	if err := notifications.NewStore(db).EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("ensure notifications indexes: %v", err)
	}

	topo := topology.Default.Suffixed(fmt.Sprintf("-it-%d", time.Now().UnixNano()))
	topo.RetryTTLs = []time.Duration{150 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond}
	testConn, err := amqp.Dial(amqpURL)
	if err != nil {
		t.Fatalf("dial rabbitmq: %v", err)
	}
	// The runner's own cleanup (cancel + wait) runs first — LIFO — so the
	// consumers are detached by the time the topology is deleted.
	t.Cleanup(func() {
		if ch, err := testConn.Channel(); err == nil {
			_ = topo.Delete(ch)
			_ = ch.Close()
		}
		_ = testConn.Close()
	})

	return &itEnv{
		amqpURL: amqpURL, mongoDB: db, topo: topo, testConn: testConn,
		hist: topo.Queues[0].Name, notif: topo.Queues[1].Name,
	}
}

// channel opens one channel on the shared test connection.
func (e *itEnv) channel() (*amqp.Channel, error) {
	return e.testConn.Channel()
}

// startRunner runs r.Run in the background and returns its cancel func and
// a wait func (idempotent; also safe for the cleanup below). The cleanup
// stops the runner even when the test body never waits.
func (e *itEnv) startRunner(t *testing.T, handlers map[string]Handler) (context.CancelFunc, func() error) {
	t.Helper()
	r, err := New(Config{URL: e.amqpURL, Topology: e.topo, Handlers: handlers})
	if err != nil {
		t.Fatalf("New runner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	var runErr error
	go func() { runErr = r.Run(ctx); close(returned) }()
	wait := func() error {
		<-returned
		return runErr
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-returned:
		case <-time.After(10 * time.Second):
			t.Error("runner did not stop after cancel")
		}
		if err := wait(); err != nil {
			t.Errorf("runner Run = %v, want nil", err)
		}
	})
	return cancel, wait
}

// waitReady blocks until the runner has declared its topology far enough
// for publishing to be safe: EVERY declared queue exists — main queues,
// their whole retry ladder, and the DLQ. The retry ladder is declared
// after a queue's bindings, so this guarantees the bindings are in place;
// checking less (exchange only, or just the main queues) races the
// bindings, and a topic exchange silently drops unroutable messages.
func (e *itEnv) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(itDeadline)
	for time.Now().Before(deadline) {
		ch, err := e.channel()
		if err == nil {
			ok := true
			for _, q := range e.topo.QueueNames() {
				if _, qerr := ch.QueueDeclarePassive(q, true, false, false, false, nil); qerr != nil {
					ok = false
					break
				}
			}
			_ = ch.Close()
			if ok {
				return
			}
		}
		time.Sleep(itPoll)
	}
	t.Fatal("topology was not declared before the deadline")
}

func (e *itEnv) publish(t *testing.T, routingKey string, body []byte, headers amqp.Table) {
	t.Helper()
	ch, err := e.channel()
	if err != nil {
		t.Fatalf("test channel: %v", err)
	}
	defer ch.Close()
	pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.PublishWithContext(pctx, e.topo.EventsExchange, routingKey, false, false,
		amqp.Publishing{Body: body, Headers: headers, DeliveryMode: amqp.Persistent}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func (e *itEnv) publishEnvelope(t *testing.T, env publisher.Envelope) {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	e.publish(t, env.Type, body, nil)
}

// queueDepth reads a queue's ready-message count with a passive declare.
func (e *itEnv) queueDepth(t *testing.T, queue string) int {
	t.Helper()
	ch, err := e.channel()
	if err != nil {
		t.Fatalf("test channel: %v", err)
	}
	defer ch.Close()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatalf("passive declare %s: %v", queue, err)
	}
	return q.Messages
}

func (e *itEnv) count(t *testing.T, collection string) int64 {
	t.Helper()
	n, err := e.mongoDB.Collection(collection).CountDocuments(t.Context(), bson.M{})
	if err != nil {
		t.Fatalf("count %s: %v", collection, err)
	}
	return n
}

func waitUntil(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(itDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(itPoll)
	}
	t.Fatalf("condition not met before deadline: %s", desc)
}

func itEnvelope(id, eventType string) publisher.Envelope {
	return publisher.Envelope{
		EventID: id, TenantID: "t-47-it", Type: eventType, Version: 1,
		EntityType: "requisition", EntityID: "req-" + id,
		OccurredAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC),
		ActorID:    "user-9", TraceID: "trace-47",
		Payload: json.RawMessage(fmt.Sprintf(`{"requester_id":"user-%s","number":"PR-1"}`, id)),
	}
}

// flakyHandler fails the first fail attempts with a transient error, then
// delegates — simulating a Mongo/network blip for the retry-ladder tests.
type flakyHandler struct {
	mu       sync.Mutex
	fail     int
	attempts int
	delegate Handler
}

func (f *flakyHandler) Apply(ctx context.Context, ev history.Event) (bool, error) {
	f.mu.Lock()
	f.attempts++
	n := f.attempts
	f.mu.Unlock()
	if n <= f.fail {
		return false, errors.New("transient: mongo connection refused")
	}
	return f.delegate.Apply(ctx, ev)
}

func (f *flakyHandler) attemptsSoFar() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// blockingHandler parks one delivery until the test releases it (or the
// delivery context ends — like a real store call under shutdown).
type blockingHandler struct {
	enter    chan struct{}
	release  chan struct{}
	delegate Handler
}

func (b *blockingHandler) Apply(ctx context.Context, ev history.Event) (bool, error) {
	select {
	case b.enter <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return b.delegate.Apply(ctx, ev)
}

// TestConsumerEndToEndIdempotent is the main acceptance test: an envelope
// published to procurement.events is projected into events + audit_log +
// notifications by the running consumers; a duplicate delivery of the same
// event_id produces no second document anywhere and is acked (proven by the
// next, later event being processed); the notifications recipient follows
// requester_id with submitted_by fallback.
func TestConsumerEndToEndIdempotent(t *testing.T) {
	env := newItEnv(t)
	env.startRunner(t, map[string]Handler{
		env.hist:  history.NewStore(env.mongoDB),
		env.notif: notifications.NewStore(env.mongoDB),
	})
	env.waitReady(t)

	approved := itEnvelope("ev-approved-1", "pr.approved")
	env.publishEnvelope(t, approved)
	waitUntil(t, "approved event projected", func() bool {
		return env.count(t, history.EventsCollectionName) == 1 &&
			env.count(t, history.AuditCollectionName) == 1 &&
			env.count(t, notifications.NotificationsCollectionName) == 1
	})

	// Duplicate delivery of the same event_id (PRD §14.8): no second effect.
	env.publishEnvelope(t, approved)
	// A later, distinct event proves the duplicate in front of it settled
	// (FIFO per queue, one serial consumer); a submitted_by-only payload
	// pins the recipient fallback.
	submitted := itEnvelope("ev-submitted-2", "pr.submitted")
	submitted.Payload = json.RawMessage(`{"submitted_by":"user-2","number":"PR-1"}`)
	env.publishEnvelope(t, submitted)
	waitUntil(t, "submitted event projected after the duplicate", func() bool {
		return env.count(t, history.EventsCollectionName) == 2 &&
			env.count(t, notifications.NotificationsCollectionName) == 2
	})

	// Both queues drained: every delivery, duplicates included, was acked.
	waitUntil(t, "queues drained", func() bool {
		return env.queueDepth(t, env.hist) == 0 && env.queueDepth(t, env.notif) == 0
	})
	time.Sleep(300 * time.Millisecond) // an unacked redelivery would surface by now
	for collection, want := range map[string]int64{
		history.EventsCollectionName:              2,
		history.AuditCollectionName:               1, // pr.approved only
		notifications.NotificationsCollectionName: 2,
	} {
		if got := env.count(t, collection); got != want {
			t.Fatalf("%s = %d, want %d (duplicate was not deduped)", collection, got, want)
		}
	}

	// Recipient derivation stored on the documents.
	var notif struct {
		EventID     string `bson:"event_id"`
		RecipientID string `bson:"recipient_id"`
	}
	if err := env.mongoDB.Collection(notifications.NotificationsCollectionName).
		FindOne(t.Context(), bson.M{"event_id": approved.EventID}).Decode(&notif); err != nil {
		t.Fatalf("find notification: %v", err)
	}
	if notif.RecipientID != "user-ev-approved-1" {
		t.Fatalf("approved recipient_id = %q, want requester_id", notif.RecipientID)
	}
	if err := env.mongoDB.Collection(notifications.NotificationsCollectionName).
		FindOne(t.Context(), bson.M{"event_id": submitted.EventID}).Decode(&notif); err != nil {
		t.Fatalf("find submitted notification: %v", err)
	}
	if notif.RecipientID != "user-2" {
		t.Fatalf("submitted recipient_id = %q, want submitted_by fallback", notif.RecipientID)
	}
}

// TestRetryLadderThenSuccess pins the transient-failure path: each failure
// publishes a copy to the next TTL tier and acks the original; after the
// blip clears the redelivery applies exactly once (at-least-once delivery,
// one effect).
func TestRetryLadderThenSuccess(t *testing.T) {
	env := newItEnv(t)
	flaky := &flakyHandler{fail: 2, delegate: history.NewStore(env.mongoDB)}
	env.startRunner(t, map[string]Handler{env.hist: flaky})
	env.waitReady(t)

	env.publishEnvelope(t, itEnvelope("ev-flaky-1", "grn.posted")) // binds only documents.history (#)
	waitUntil(t, "event applied after retries", func() bool {
		return env.count(t, history.EventsCollectionName) == 1
	})
	if got := flaky.attemptsSoFar(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (1 initial + 2 retries)", got)
	}
	waitUntil(t, "ladder drained", func() bool {
		for _, q := range env.topo.QueueNames() {
			if env.queueDepth(t, q) != 0 {
				return false
			}
		}
		return true
	})
	if got := env.count(t, history.EventsCollectionName); got != 1 {
		t.Fatalf("events = %d, want 1 (redelivery was not deduped)", got)
	}
}

// TestRetryExhaustionDeadLetters pins the max-attempts path: after
// MaxRetries failed tiers the delivery is nacked without requeue and lands
// in procurement.dlq (PRD §14.5), leaving the retry queues empty.
func TestRetryExhaustionDeadLetters(t *testing.T) {
	env := newItEnv(t)
	always := &flakyHandler{fail: 1 << 30, delegate: history.NewStore(env.mongoDB)}
	env.startRunner(t, map[string]Handler{env.hist: always})
	env.waitReady(t)

	sent := itEnvelope("ev-doomed-1", "grn.posted")
	env.publishEnvelope(t, sent)
	waitUntil(t, "message reached the DLQ", func() bool {
		return env.queueDepth(t, env.topo.DLQQueue) == 1
	})
	if got := always.attemptsSoFar(); got != topology.MaxRetries+1 {
		t.Fatalf("attempts = %d, want %d (1 initial + 3 retries)", got, topology.MaxRetries+1)
	}
	for _, q := range env.topo.QueueNames() {
		if q == env.topo.DLQQueue {
			continue
		}
		if d := env.queueDepth(t, q); d != 0 {
			t.Fatalf("queue %s depth = %d, want 0", q, d)
		}
	}
	if got := env.count(t, history.EventsCollectionName); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}

	// The dead-lettered body is the original message: admin-visible, replayable.
	ch, err := env.channel()
	if err != nil {
		t.Fatalf("test channel: %v", err)
	}
	msg, _, err := ch.Get(env.topo.DLQQueue, true)
	if err != nil {
		t.Fatalf("get from dlq: %v", err)
	}
	_ = ch.Close()
	var got publisher.Envelope
	if err := json.Unmarshal(msg.Body, &got); err != nil || got.EventID != sent.EventID {
		t.Fatalf("dlq body = %s (event_id %q), want the original %s", msg.Body, got.EventID, sent.EventID)
	}
}

// TestPermanentInvalidGoesStraightToDLQ pins the no-retry rule for messages
// that can never succeed — undecodable bodies and invalid envelopes — and
// proves they do not block the deliveries behind them.
func TestPermanentInvalidGoesStraightToDLQ(t *testing.T) {
	env := newItEnv(t)
	env.startRunner(t, map[string]Handler{env.hist: history.NewStore(env.mongoDB)})
	env.waitReady(t)

	env.publish(t, "grn.posted", []byte(`{"event_id":`), nil) // undecodable
	broken := itEnvelope("ev-broken-1", "grn.posted")
	broken.EntityType = "" // fails validation: no aggregate identity
	env.publishEnvelope(t, broken)
	valid := itEnvelope("ev-valid-1", "grn.posted")
	env.publishEnvelope(t, valid)

	waitUntil(t, "valid event applied behind the poison messages", func() bool {
		return env.count(t, history.EventsCollectionName) == 1
	})
	waitUntil(t, "both poison messages dead-lettered", func() bool {
		return env.queueDepth(t, env.topo.DLQQueue) == 2
	})
	if got := env.count(t, history.EventsCollectionName); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
}

// TestShutdownRedeliversWithoutDoubleEffect pins the clean-restart
// acceptance: SIGTERM (ctx cancel) during an in-flight delivery interrupts
// the handler, the unacknowledged delivery returns to its queue, Run
// returns nil, and after a restart the redelivery applies exactly once —
// the unique event_id index absorbs the double delivery (PRD §14.8).
func TestShutdownRedeliversWithoutDoubleEffect(t *testing.T) {
	env := newItEnv(t)
	blocking := &blockingHandler{
		enter:    make(chan struct{}),
		release:  make(chan struct{}),
		delegate: history.NewStore(env.mongoDB),
	}
	cancel1, wait1 := env.startRunner(t, map[string]Handler{env.hist: blocking})
	env.waitReady(t)

	env.publishEnvelope(t, itEnvelope("ev-restart-1", "grn.posted"))
	select {
	case <-blocking.enter: // delivery is in flight, unacked, nothing written
	case <-time.After(itDeadline):
		t.Fatal("delivery never reached the handler")
	}
	if got := env.count(t, history.EventsCollectionName); got != 0 {
		t.Fatalf("events before shutdown = %d, want 0 (handler still blocked)", got)
	}

	cancel1() // SIGTERM: the handler's context ends, the delivery is requeued
	if err := wait1(); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	waitUntil(t, "delivery returned to the queue", func() bool {
		return env.queueDepth(t, env.hist) == 1
	})

	// Restart: the redelivered event applies exactly once.
	env.startRunner(t, map[string]Handler{env.hist: history.NewStore(env.mongoDB)})
	waitUntil(t, "event applied after restart", func() bool {
		return env.count(t, history.EventsCollectionName) == 1
	})
	time.Sleep(300 * time.Millisecond)
	if got := env.count(t, history.EventsCollectionName); got != 1 {
		t.Fatalf("events = %d, want 1 (redelivery was not deduped)", got)
	}
	waitUntil(t, "queue drained after restart", func() bool {
		return env.queueDepth(t, env.hist) == 0
	})
}
