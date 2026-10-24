package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/outbox"
	"github.com/luksluki/eprocurement/services/worker/topology"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePublisher records envelopes and fails the first failN Publish calls.
type fakePublisher struct {
	mu        sync.Mutex
	failN     int
	failed    int
	envelopes []Envelope
}

func (f *fakePublisher) Publish(_ context.Context, env Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed < f.failN {
		f.failed++
		return errors.New("fake broker down")
	}
	f.envelopes = append(f.envelopes, env)
	return nil
}

func (f *fakePublisher) received() []Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Envelope(nil), f.envelopes...)
}

// fakeStore is the loop's persistence side with scriptable poll failures.
type fakeStore struct {
	mu         sync.Mutex
	rows       []outbox.PendingEvent
	marked     map[string]bool
	pendingErr error // returned by Pending while set
	markCalls  [][]string
}

func newFakeStore(rows ...outbox.PendingEvent) *fakeStore {
	return &fakeStore{rows: rows, marked: map[string]bool{}}
}

func (f *fakeStore) Pending(_ context.Context, limit int) ([]outbox.PendingEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	out := []outbox.PendingEvent{}
	for _, r := range f.rows {
		if !f.marked[r.ID] && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkPublished(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markCalls = append(f.markCalls, append([]string(nil), ids...))
	for _, id := range ids {
		f.marked[id] = true
	}
	return nil
}

func row(id, tenantID, eventType string) outbox.PendingEvent {
	return outbox.PendingEvent{
		ID: id, TenantID: tenantID, EventType: eventType, Version: 1,
		Payload: json.RawMessage(`{"number":"PR-000001"}`),
		TraceID: "trace-45", EntityType: "requisition",
		EntityID:   "eeeeeeee-0000-0000-0000-0000000000cc",
		OccurredAt: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
	}
}

// Envelope is assembled 1:1 from the row columns with exactly the §14.5
// field set and names.
func TestEnvelopeFromRow(t *testing.T) {
	env := EnvelopeFromRow(row("ev-1", "t-1", "pr.submitted"))
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"event_id":"ev-1","tenant_id":"t-1","type":"pr.submitted","version":1,` +
		`"occurred_at":"2026-10-25T08:00:00Z","actor_id":"","trace_id":"trace-45",` +
		`"payload":{"number":"PR-000001"}}`
	if string(body) != want {
		t.Fatalf("envelope =\n%s\nwant\n%s", body, want)
	}
}

// AC (publisher retries safely, published_at NULL until confirm): a failed
// publish leaves the row unmarked; the next tick publishes and marks it.
func TestLoopRetriesFailedPublish(t *testing.T) {
	store := newFakeStore(row("ev-1", "t-1", "pr.submitted"))
	pub := &fakePublisher{failN: 1} // first publish fails, second succeeds
	loop := NewLoop(store, pub, testLogger(), time.Hour, 50)

	loop.pollOnce(context.Background())
	if got := pub.received(); len(got) != 0 {
		t.Fatalf("published during broker failure = %+v, want none", got)
	}
	if len(store.markCalls) != 0 {
		t.Fatalf("marked rows during broker failure = %v, want none", store.markCalls)
	}
	rows, err := store.Pending(context.Background(), 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("row left pending = (%+v, %v), want the failed row back", rows, err)
	}

	loop.pollOnce(context.Background())
	got := pub.received()
	if len(got) != 1 || got[0].EventID != "ev-1" || got[0].Type != "pr.submitted" {
		t.Fatalf("published = %+v, want the retried envelope", got)
	}
	if len(store.markCalls) != 1 || store.markCalls[0][0] != "ev-1" {
		t.Fatalf("mark calls = %v, want [ev-1] after success", store.markCalls)
	}
}

// AC (batch semantics): a failure stops the batch — the confirmed prefix is
// marked, the tail retries on the next tick in order.
func TestLoopStopsBatchOnPublishFailure(t *testing.T) {
	store := newFakeStore(row("ev-1", "t-1", "pr.approved"), row("ev-2", "t-2", "pr.submitted"))
	pub := &fakePublisher{failN: 1} // fails ev-1's publish
	loop := NewLoop(store, pub, testLogger(), time.Hour, 50)

	loop.pollOnce(context.Background())
	if got := pub.received(); len(got) != 0 {
		t.Fatalf("published = %+v, want none — ev-1 failed and ev-2 must wait", got)
	}
	if len(store.markCalls) != 0 {
		t.Fatalf("marked = %v, want nothing marked while ev-1 is unpublished", store.markCalls)
	}

	loop.pollOnce(context.Background())
	got := pub.received()
	if len(got) != 2 || got[0].EventID != "ev-1" || got[1].EventID != "ev-2" {
		t.Fatalf("published = %+v, want ev-1 then ev-2 after recovery", got)
	}
}

// AC (poll errors are non-fatal): the running loop survives a failing poll —
// here the fake store erroring for a while — and publishes once the poll
// succeeds again. Run is the real loop, not a single poll.
func TestLoopRunToleratesPollErrors(t *testing.T) {
	store := newFakeStore(row("ev-1", "t-1", "pr.submitted"))
	store.pendingErr = errors.New("fake: schema \"procurement\" does not exist")
	pub := &fakePublisher{}
	loop := NewLoop(store, pub, testLogger(), 5*time.Millisecond, 50)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = loop.Run(ctx); close(done) }()

	// The failing polls must not exit the loop: clear the error and the
	// still-running loop publishes on a later tick.
	time.Sleep(30 * time.Millisecond)
	store.mu.Lock()
	store.pendingErr = nil
	store.mu.Unlock()

	waitFor(t, 2*time.Second, func() bool { return len(pub.received()) == 1 })
	if got := pub.received(); len(got) != 1 || got[0].EventID != "ev-1" {
		t.Fatalf("published after recovery = %+v, want ev-1 — the loop died on the poll error", got)
	}
	cancel()
	<-done // Run returns nil on cancellation (graceful shutdown)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// openTestDB applies the embedded procurement migrations and opens a pool;
// skipped without TEST_DATABASE_URL (same harness shape as the other suites).
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	migCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		t.Fatalf("open migration pool: %v", err)
	}
	if _, err := dbmigrate.Apply(t.Context(), migPool, "procurement", migrations.FS()); err != nil {
		migPool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	migPool.Close()

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// scratchDatabase creates an empty database (no migrations applied) so a test
// can watch the loop hit a missing schema without touching the shared test
// database. Skipped when the connection user cannot create databases.
func scratchDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := fmt.Sprintf("eproc_test_45_scratch")
	admin := *u
	admin.Path = "/postgres"
	pool, err := pgxpool.New(context.Background(), admin.String())
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name); err != nil {
		pool.Close()
		t.Skipf("cannot drop scratch database (%v); skipping", err)
	}
	if _, err := pool.Exec(context.Background(), `CREATE DATABASE `+name); err != nil {
		pool.Close()
		t.Skipf("create database not permitted (%v); skipping", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		pool.Close()
	})
	scratch := *u
	scratch.Path = "/" + name
	return scratch.String()
}

// applyProcurementMigrations applies the embedded migrations on dsn (the
// scratch database); the simple-protocol pool is dbmigrate's requirement.
func applyProcurementMigrations(t *testing.T, dsn string) {
	t.Helper()
	migCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse scratch dsn: %v", err)
	}
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		t.Fatalf("open scratch migration pool: %v", err)
	}
	defer migPool.Close()
	if _, err := dbmigrate.Apply(t.Context(), migPool, "procurement", migrations.FS()); err != nil {
		t.Fatalf("apply migrations to scratch: %v", err)
	}
}

// AC (poll errors are non-fatal, fresh-volume scenario): the worker boots
// with only postgres+rabbitmq healthy — procurement, the sole migration
// applier, has not applied them yet — so the loop's polls fail with a
// missing-schema error. The loop must log and keep ticking (a fatal loop
// would kill the container and the compose gate), then drain rows once the
// migrations land.
func TestLoopRunAgainstUnmigratedDatabase(t *testing.T) {
	dsn := scratchDatabase(t)
	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	defer db.Close()

	pub := &fakePublisher{}
	loop := NewLoop(OutboxDB{DB: db}, pub, testLogger(), 5*time.Millisecond, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = loop.Run(ctx); close(done) }()

	// Polls are failing against the unmigrated database (schema missing).
	// A fatal loop would have exited here; prove liveness by migrating and
	// watching the still-running loop pick the row up.
	time.Sleep(50 * time.Millisecond)
	applyProcurementMigrations(t, dsn)

	const tenant45 = "eeeeeeee-0000-0000-0000-000000000045"
	ev := outbox.Event{
		ID: "eeeeeeee-0000-0000-0000-000000000046", TenantID: tenant45,
		EventType: "pr.submitted", Payload: map[string]string{"number": "PR-000001"},
		EntityType: "requisition", EntityID: "eeeeeeee-0000-0000-0000-000000000047",
		OccurredAt: time.Now().UTC(),
	}
	if err := tenant.WithTenant(context.Background(), db, tenant45, func(tx pgx.Tx) error {
		return outbox.AppendTx(context.Background(), tx, ev)
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return len(pub.received()) == 1 })
	got := pub.received()
	if len(got) != 1 || got[0].EventID != ev.ID || got[0].Type != "pr.submitted" || got[0].TenantID != tenant45 {
		t.Fatalf("published = %+v, want the row appended after migration", got)
	}
	waitFor(t, 5*time.Second, func() bool {
		var n int
		_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`SELECT count(*) FROM procurement.outbox WHERE id = $1 AND published_at IS NOT NULL`,
				ev.ID).Scan(&n)
		})
		return n == 1
	})
	cancel()
	<-done
}

// AC (end-to-end publish): a real outbox row flows through the loop with a
// real confirm-mode AMQP publisher — the message lands on the exchange with
// the §14.5 envelope body, routing key = event type, persistent delivery —
// and the row is marked published. Skipped without a local RabbitMQ
// (TEST_RABBITMQ_URL; CI has no RabbitMQ service and is not changed).
func TestAMQPPublisherEndToEnd(t *testing.T) {
	amqpURL := os.Getenv("TEST_RABBITMQ_URL")
	if amqpURL == "" {
		t.Skip("TEST_RABBITMQ_URL not set; skipping integration test")
	}
	db := openTestDB(t)
	const tenant45 = "eeeeeeee-0000-0000-0000-000000000045"
	const evID = "eeeeeeee-0000-0000-0000-000000000048"
	t.Cleanup(func() {
		_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`DELETE FROM procurement.outbox WHERE tenant_id = $1`, tenant45)
			return err
		})
	})
	// The batch poll reads every tenant's unpublished rows; other suites'
	// leftovers in the shared test database must not ride this batch.
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`UPDATE procurement.outbox SET published_at = now() WHERE published_at IS NULL`)
		return err
	}); err != nil {
		t.Fatalf("quarantine pre-existing rows: %v", err)
	}

	pub, err := Connect(context.Background(), amqpURL, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pub.Close()

	// Capture the published message through a short-lived queue bound to
	// everything. The production consumer queues exist since #46, but the
	// consumers themselves are #47 — nothing drains them yet (§14.5).
	ch, err := pub.conn.Channel()
	if err != nil {
		t.Fatalf("test channel: %v", err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatalf("declare capture queue: %v", err)
	}
	if err := ch.QueueBind(q.Name, "#", topology.EventsExchange, false, nil); err != nil {
		t.Fatalf("bind capture queue: %v", err)
	}

	// Connect declares the full §14.5 topology (issue #46): passively
	// verify both exchanges and every queue — mains, retry tiers, DLQ —
	// exist after the first connect; the queue arguments themselves are
	// pinned by the topology unit tests.
	for _, check := range []struct {
		name string
		do   func() error
	}{
		{topology.EventsExchange, func() error {
			return ch.ExchangeDeclarePassive(topology.EventsExchange, "topic", true, false, false, false, nil)
		}},
		{topology.DLXExchange, func() error {
			return ch.ExchangeDeclarePassive(topology.DLXExchange, "topic", true, false, false, false, nil)
		}},
	} {
		if err := check.do(); err != nil {
			t.Fatalf("passive declare %s: %v", check.name, err)
		}
	}
	for _, name := range topology.Default.QueueNames() {
		if _, err := ch.QueueDeclarePassive(name, true, false, false, false, nil); err != nil {
			t.Fatalf("passive declare queue %s: %v", name, err)
		}
	}

	// One real row, committed unmarked (the crash-between-commit-and-mark
	// start state), drained by one loop poll.
	ev := outbox.Event{
		ID: evID, TenantID: tenant45, EventType: "pr.submitted",
		Payload: map[string]any{"number": "PR-000001", "total_cents": 15450000},
		ActorID: "eeeeeeee-0000-0000-0000-0000000000aa", TraceID: "trace-45-e2e",
		EntityType: "requisition", EntityID: "eeeeeeee-0000-0000-0000-000000000047",
		OccurredAt: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
	}
	if err := tenant.WithTenant(context.Background(), db, tenant45, func(tx pgx.Tx) error {
		return outbox.AppendTx(context.Background(), tx, ev)
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	loop := NewLoop(OutboxDB{DB: db}, pub, testLogger(), time.Hour, 50)
	loop.pollOnce(context.Background())

	msg, ok := fetchMessage(t, ch, q.Name, evID)
	if !ok {
		t.Fatal("no message arrived on the capture queue")
	}
	if msg.RoutingKey != "pr.submitted" {
		t.Fatalf("routing key = %s, want pr.submitted", msg.RoutingKey)
	}
	if msg.DeliveryMode != amqp.Persistent {
		t.Fatalf("delivery mode = %d, want persistent (%d)", msg.DeliveryMode, amqp.Persistent)
	}
	if msg.MessageId != evID {
		t.Fatalf("message id = %s, want the event id", msg.MessageId)
	}
	var env Envelope
	if err := json.Unmarshal(msg.Body, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v (%s)", err, msg.Body)
	}
	if env.EventID != evID || env.TenantID != tenant45 || env.Type != "pr.submitted" ||
		env.Version != 1 || env.ActorID != ev.ActorID || env.TraceID != "trace-45-e2e" ||
		env.Payload == nil || string(env.Payload) == "" {
		t.Fatalf("envelope = %+v (payload %s)", env, env.Payload)
	}
	if !env.OccurredAt.Equal(ev.OccurredAt) {
		t.Fatalf("occurred_at = %v, want %v", env.OccurredAt, ev.OccurredAt)
	}

	// And the row the message came from is now marked published.
	var published int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.outbox WHERE id = $1 AND published_at IS NOT NULL`,
			evID).Scan(&published)
	}); err != nil || published != 1 {
		t.Fatalf("row marked published = (%d, %v), want 1", published, err)
	}

	// The published messages also landed in the production consumer queues
	// (history binds #; notifications takes its subset) that nothing
	// consumes until #47 — purge them so this suite leaves the shared dev
	// broker clean. Explicit, not t.Cleanup: cleanup runs after the deferred
	// pub.Close() has already torn the channel down.
	for _, name := range topology.Default.QueueNames() {
		if _, err := ch.QueuePurge(name, false); err != nil {
			t.Fatalf("purge %s: %v", name, err)
		}
	}
}

// fetchMessage waits for the message with the wanted id, acking (and
// skipping) anything else that rode the batch — parallel suites share the
// test database, so the publisher may legitimately drain their rows too.
func fetchMessage(t *testing.T, ch *amqp.Channel, queue, wantID string) (amqp.Delivery, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatalf("basic.get: %v", err)
		}
		if ok {
			if msg.MessageId == wantID {
				return msg, true
			}
			continue // another suite's row riding the same batch
		}
		time.Sleep(10 * time.Millisecond)
	}
	return amqp.Delivery{}, false
}

// Failure path: connecting to a broker that is not there fails (the boot
// retry/backoff in main and the loop's log-and-continue cover the runtime
// twin).
func TestConnectFailure(t *testing.T) {
	if _, err := Connect(context.Background(), "amqp://127.0.0.1:1/", time.Second); err == nil {
		t.Fatal("connect to a closed port succeeded, want error")
	}
}
