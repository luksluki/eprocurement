// Package integration hosts the procure-to-pay cross-service suite (#58,
// epic #11 / milestone M4; ADR-0002 CI stage 3): one story driven through
// the REAL gRPC procurement API in-process, whose outbox rows are drained by
// the REAL worker publisher loop over RabbitMQ into the REAL worker consumer
// runner, projecting into the REAL MongoDB event and audit stores — every
// hop against genuine infrastructure, only the HTTP edge (gateway/web)
// excluded.
//
// Isolation model (the flakiness guards, each pinned by a decision):
//
//   - Per RUN: one scratch PostgreSQL database (dropped at teardown,
//     migrations applied through dbmigrate.Apply — the same embedded set
//     services/procurement/cmd/migrate applies), one scratch MongoDB
//     database (dropped at teardown), and one suffixed copy of the
//     production RabbitMQ topology (topology.Suffixed + Delete). The scratch
//     database is what neutralizes the race with a live compose worker: the
//     shared-stack worker's outbox.Pending has no SKIP LOCKED, but it reads
//     the compose database, never this one.
//   - Per TEST: a fresh tenant_id (and fresh user ids). Every assertion on
//     MongoDB filters by tenant_id and every Postgres cleanup deletes by
//     tenant_id, so tests never see each other's rows or documents — the
//     alternative (fixed tenants + Mongo collection drops per test) would
//     couple the file execution order to the assertions. Cleanup runs
//     BEFORE and AFTER each test, FK-safe child→parent over every
//     procurement table the flow writes.
//
// The suite skips cleanly unless TEST_DATABASE_URL, TEST_MONGO_URI, and
// TEST_RABBITMQ_URL are all set (the consumer_integration_test.go
// convention), so plain `go test ./...` stays green without infrastructure.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/notifications"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/dashboard"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/match"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/server"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
	"github.com/luksluki/eprocurement/services/worker/consumer"
	"github.com/luksluki/eprocurement/services/worker/publisher"
	"github.com/luksluki/eprocurement/services/worker/topology"
)

const (
	// pollInterval is the assert-poll cadence against MongoDB.
	pollInterval = 50 * time.Millisecond
	// waitDeadline bounds every eventually-style assert (CI-safe; the
	// consumer_integration_test.go itDeadline convention).
	waitDeadline = 15 * time.Second
	// retryTTL shortens the topology's retry ladder so a failing delivery
	// would settle quickly (nothing in this suite fails on purpose at the
	// consumer, but the ladder stays production-shaped).
	retryTTL = 150 * time.Millisecond
	// loopInterval drives the harness publisher Loop — NewLoop takes the
	// interval explicitly; 2 s/50 is only main.go's pinned default config,
	// not a property of the Loop.
	loopInterval = 50 * time.Millisecond
	// loopBatch is the harness Loop batch size (main.go's default).
	loopBatch = 50
	// workerStop bounds the shared worker halves' shutdown at teardown.
	workerStop = 10 * time.Second
	// dropTimeout bounds one DROP DATABASE attempt (see dropScratch: a
	// single wedged backend anywhere hangs every drop on the server).
	dropTimeout = 5 * time.Second
)

// runEnv holds the per-RUN fixtures shared by every test in the package.
type runEnv struct {
	db          *pgxpool.Pool // the scratch database, migrated
	adminDB     *pgxpool.Pool // maintenance-db pool; drops the scratch database
	scratchName string

	mongoClient *mongo.Client
	mongoDB     *mongo.Database

	topo     topology.Topology // suffixed copy of topology.Default
	testConn *amqp.Connection  // passive checks + topology teardown

	cancel     context.CancelFunc
	loopDone   chan struct{}
	runnerDone chan struct{}
}

var (
	runOnce sync.Once
	shared  *runEnv
	runErr  error
)

// TestMain tears the shared fixtures down AFTER every test has finished —
// a per-test t.Cleanup could not hold run-scoped state.
func TestMain(m *testing.M) {
	code := m.Run()
	if shared != nil {
		shared.teardown()
	}
	os.Exit(code)
}

// sharedRun lazily builds the run fixtures once and returns them; every test
// calls it, and the first call wins.
func sharedRun(t *testing.T) *runEnv {
	t.Helper()
	pgDSN := os.Getenv("TEST_DATABASE_URL")
	mongoURI := os.Getenv("TEST_MONGO_URI")
	amqpURL := os.Getenv("TEST_RABBITMQ_URL")
	if pgDSN == "" || mongoURI == "" || amqpURL == "" {
		t.Skip("TEST_DATABASE_URL/TEST_MONGO_URI/TEST_RABBITMQ_URL not set; skipping integration test")
	}
	runOnce.Do(func() { shared, runErr = initRun(pgDSN, mongoURI, amqpURL) })
	if runErr != nil {
		t.Fatalf("p2p run harness: %v", runErr)
	}
	return shared
}

// initRun builds the per-run fixtures: scratch databases, suffixed topology,
// and the two real worker halves (publisher Loop + consumer Runner) running
// until teardown.
func initRun(pgDSN, mongoURI, amqpURL string) (*runEnv, error) {
	ctx := context.Background()
	e := &runEnv{
		scratchName: fmt.Sprintf("eproc_p2p_it_%d", time.Now().UnixNano()),
	}

	// --- scratch PostgreSQL: drop-if-exists, create, migrate -------------
	u, err := url.Parse(pgDSN)
	if err != nil {
		return nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	admin := *u
	admin.Path = "/postgres"
	e.adminDB, err = pgxpool.New(ctx, admin.String())
	if err != nil {
		return nil, fmt.Errorf("open admin pool: %w", err)
	}
	// The name is fresh (nanos), so this is normally a catalog-only no-op;
	// bounded anyway — a drop must never hang the suite (dropScratch).
	dropCtx, dropCancel := context.WithTimeout(ctx, dropTimeout)
	_, dropErr := e.adminDB.Exec(dropCtx, `DROP DATABASE IF EXISTS `+e.scratchName)
	dropCancel()
	if dropErr != nil {
		e.adminDB.Close()
		return nil, fmt.Errorf("drop scratch database (CREATE DATABASE privilege required): %w", dropErr)
	}
	if _, err := e.adminDB.Exec(ctx, `CREATE DATABASE `+e.scratchName); err != nil {
		e.adminDB.Close()
		return nil, fmt.Errorf("create scratch database %s: %w", e.scratchName, err)
	}
	scratch := *u
	scratch.Path = "/" + e.scratchName
	migCfg, err := pgxpool.ParseConfig(scratch.String())
	if err != nil {
		e.dropScratch(e.scratchName) // CREATE DATABASE already ran — drop, then close
		e.adminDB.Close()
		return nil, fmt.Errorf("parse scratch dsn: %w", err)
	}
	// dbmigrate's requirement: multi-statement files need the simple
	// protocol (the cmd/migrate wiring).
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(ctx, migCfg)
	if err != nil {
		e.dropScratch(e.scratchName) // CREATE DATABASE already ran — drop, then close
		e.adminDB.Close()
		return nil, fmt.Errorf("open migration pool: %w", err)
	}
	_, migErr := dbmigrate.Apply(ctx, migPool, "procurement", migrations.FS())
	migPool.Close()
	if migErr != nil {
		e.dropScratch(e.scratchName) // CREATE DATABASE already ran — drop, then close
		e.adminDB.Close()
		return nil, fmt.Errorf("apply migrations: %w", migErr)
	}
	if e.db, err = pgxpool.New(ctx, scratch.String()); err != nil {
		e.dropScratch(e.scratchName) // CREATE DATABASE already ran — drop, then close
		e.adminDB.Close()
		return nil, fmt.Errorf("open scratch pool: %w", err)
	}

	// --- scratch MongoDB: create, ensure the projection indexes ----------
	mctx, mcancel := context.WithTimeout(ctx, 10*time.Second)
	defer mcancel()
	if e.mongoClient, err = mongo.Connect(mctx, options.Client().ApplyURI(mongoURI)); err != nil {
		e.teardown()
		return nil, fmt.Errorf("connect mongo: %w", err)
	}
	if err := e.mongoClient.Ping(mctx, nil); err != nil {
		e.teardown()
		return nil, fmt.Errorf("ping mongo: %w", err)
	}
	e.mongoDB = e.mongoClient.Database(e.scratchName)
	// The cmd/worker boot order: indexes BEFORE consumption starts, so the
	// unique event_id dedupe holds from the first delivery.
	if err := history.NewStore(e.mongoDB).EnsureIndexes(ctx); err != nil {
		e.teardown()
		return nil, fmt.Errorf("ensure history indexes: %w", err)
	}
	if err := notifications.NewStore(e.mongoDB).EnsureIndexes(ctx); err != nil {
		e.teardown()
		return nil, fmt.Errorf("ensure notifications indexes: %w", err)
	}

	// --- suffixed broker topology + the test connection -------------------
	e.topo = topology.Default.Suffixed(fmt.Sprintf("-p2p-%d", time.Now().UnixNano()))
	e.topo.RetryTTLs = []time.Duration{retryTTL, retryTTL, retryTTL}
	if e.testConn, err = amqp.Dial(amqpURL); err != nil {
		e.teardown()
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}

	// --- the real worker halves against the scratch fixtures --------------
	wctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.loopDone = make(chan struct{})
	e.runnerDone = make(chan struct{})

	// Publisher: the topology-aware constructor declares the suffixed copy
	// (argument-identical to production — the PRECONDITION_FAILED hazard)
	// and publishes to its events exchange.
	pub, err := publisher.ConnectTopology(wctx, amqpURL, 5*time.Second, e.topo)
	if err != nil {
		e.teardown()
		return nil, fmt.Errorf("connect publisher: %w", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := publisher.NewLoop(publisher.OutboxDB{DB: e.db}, pub, log, loopInterval, loopBatch)
	go func() { _ = loop.Run(wctx); close(e.loopDone) }()

	// Consumer: the same two projections cmd/worker consumes, on the same
	// suffixed topology.
	runner, err := consumer.New(consumer.Config{
		URL:      amqpURL,
		Topology: e.topo,
		Handlers: map[string]consumer.Handler{
			e.topo.Queues[0].Name: history.NewStore(e.mongoDB),       // documents.history
			e.topo.Queues[1].Name: notifications.NewStore(e.mongoDB), // notifications
		},
		Logger: log,
	})
	if err != nil {
		e.teardown()
		return nil, fmt.Errorf("new consumer runner: %w", err)
	}
	go func() { _ = runner.Run(wctx); close(e.runnerDone) }()

	if err := e.waitReady(); err != nil {
		e.teardown()
		return nil, err
	}
	return e, nil
}

// waitReady blocks until the full suffixed queue set exists — mains, whole
// retry ladder, DLQ. The ladder is declared after each queue's bindings, so
// its existence proves the bindings; checking less races the bindings and a
// topic exchange silently drops unroutable messages (dev-note 047).
func (e *runEnv) waitReady() error {
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		ch, err := e.testConn.Channel()
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
				return nil
			}
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("suffixed topology was not declared before the deadline")
}

// teardown is best-effort (no *testing.T at TestMain time): stop the worker
// halves, delete the suffixed topology while no consumer holds it, drop the
// scratch databases, close the pools.
func (e *runEnv) teardown() {
	if e.cancel != nil {
		e.cancel()
		if e.loopDone != nil {
			select {
			case <-e.loopDone:
			case <-time.After(workerStop):
			}
		}
		if e.runnerDone != nil {
			select {
			case <-e.runnerDone:
			case <-time.After(workerStop):
			}
		}
	}
	if e.testConn != nil {
		if ch, err := e.testConn.Channel(); err == nil {
			_ = e.topo.Delete(ch)
			_ = ch.Close()
		}
		_ = e.testConn.Close()
	}
	if e.mongoDB != nil {
		_ = e.mongoDB.Drop(context.Background())
	}
	if e.mongoClient != nil {
		_ = e.mongoClient.Disconnect(context.Background())
	}
	if e.db != nil {
		e.db.Close()
	}
	if e.adminDB != nil {
		e.dropScratch(e.scratchName)
		e.adminDB.Close()
	}
}

// dropScratch drops one database, deadline-bounded and non-fatal. DROP
// DATABASE waits on a system-wide ProcSignal barrier, so ONE wedged backend
// anywhere on the server (observed on the dev box: a killed client's stuck
// INSERT that processed neither pg_cancel_backend nor pg_terminate_backend)
// hangs every drop indefinitely. A hung drop must never hang the suite: on
// timeout the scratch database is deliberately leaked (each run creates a
// fresh nanos-named one) with one stderr line for the log; the leak clears
// together with the wedged backend (a postgres restart).
func (e *runEnv) dropScratch(name string) {
	drop := func(stmt string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), dropTimeout)
		defer cancel()
		_, err := e.adminDB.Exec(ctx, stmt)
		return err == nil
	}
	if drop(`DROP DATABASE IF EXISTS ` + name) {
		return
	}
	// Sessions may linger against the database (a pool race): FORCE
	// terminates them, at the price of the barrier wait above.
	if drop(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`) {
		return
	}
	fmt.Fprintf(os.Stderr, "integration: leaked scratch database %s (DROP DATABASE timed out; see dropScratch)\n", name)
}

// cleanupTables is the FK-safe child→parent deletion order over EVERY
// procurement table the procure-to-pay flow writes, parameterized by
// tenant_id and executed through the RLS bypass (dev tests may not hold a
// tenant session; production code never runs deletes like these). The item
// tables carry no tenant_id by PRD §13 (tenancy derives from the parent), so
// their deletes resolve the parent ids first; the outbox has no FK at all,
// so its delete is explicit — the requisition store_test cleanup pattern.
// Vendors go last: purchase_orders and invoices hold no-cascade FKs to them.
var cleanupTables = []string{
	`DELETE FROM procurement.goods_receipt_items
	  WHERE grn_id IN (SELECT id FROM procurement.goods_receipts WHERE tenant_id = $1)`,
	`DELETE FROM procurement.goods_receipts WHERE tenant_id = $1`,
	`DELETE FROM procurement.grn_counters WHERE tenant_id = $1`,
	`DELETE FROM procurement.invoice_lines
	  WHERE invoice_id IN (SELECT id FROM procurement.invoices WHERE tenant_id = $1)`,
	`DELETE FROM procurement.invoices WHERE tenant_id = $1`,
	`DELETE FROM procurement.po_items
	  WHERE po_id IN (SELECT id FROM procurement.purchase_orders WHERE tenant_id = $1)`,
	`DELETE FROM procurement.purchase_orders WHERE tenant_id = $1`,
	`DELETE FROM procurement.po_counters WHERE tenant_id = $1`,
	`DELETE FROM procurement.decision_idempotency WHERE tenant_id = $1`,
	`DELETE FROM procurement.approval_steps
	  WHERE requisition_id IN (SELECT id FROM procurement.requisitions WHERE tenant_id = $1)`,
	`DELETE FROM procurement.approval_rules WHERE tenant_id = $1`,
	`DELETE FROM procurement.requisition_items
	  WHERE requisition_id IN (SELECT id FROM procurement.requisitions WHERE tenant_id = $1)`,
	`DELETE FROM procurement.requisitions WHERE tenant_id = $1`,
	`DELETE FROM procurement.requisition_counters WHERE tenant_id = $1`,
	`DELETE FROM procurement.match_settings WHERE tenant_id = $1`,
	`DELETE FROM procurement.outbox WHERE tenant_id = $1`,
	`DELETE FROM procurement.vendors WHERE tenant_id = $1`,
}

// cleanupTenant wipes both tenants' rows before AND after the test: after —
// so the scratch database never carries state into the next test; before —
// so a crashed prior run of the same tenant id cannot poison this one (the
// ids are fresh per test, so before is a cheap guard, not a crutch).
func cleanupTenant(t *testing.T, db *pgxpool.Pool, tenantID string) {
	t.Helper()
	err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		for _, stmt := range cleanupTables {
			if _, err := tx.Exec(context.Background(), stmt, tenantID); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cleanup tenant %s: %v", tenantID, err)
	}
}

// p2pEnv is one test's slice of the run: a fresh in-process procurement
// gRPC server, fresh tenant and user ids, and the Mongo/PG read helpers the
// assertions need.
type p2pEnv struct {
	t      *testing.T
	run    *runEnv
	client procurementv1.ProcurementClient

	tenantA   string // the story's tenant
	tenantB   string // the cross-tenant denial probe
	requester string // tenant A user holding the admin role (creates, submits, converts…)
	approver  string // tenant A second user holding the admin role (approves)

	// latencies records the observed mutation→projected-event latency per
	// step — FR-11 evidence ("every state change appears in the timeline
	// within 2 s"), reported through t.Logf; deliberately NOT a hard gate,
	// so a slow CI runner cannot flake the suite.
	latencies map[string]time.Duration
}

// newP2P builds the per-test environment: unique tenants/users (the Mongo
// isolation decision — every projection assert filters by tenant_id, so
// unique tenants make the shared scratch Mongo database safe without
// per-test collection drops), before/after cleanup, and a fresh in-process
// procurement gRPC server wired to the scratch database exactly the way
// services/procurement/cmd/procurement wires it.
func newP2P(t *testing.T) *p2pEnv {
	t.Helper()
	r := sharedRun(t)
	e := &p2pEnv{
		t:         t,
		run:       r,
		tenantA:   uuid.NewString(),
		tenantB:   uuid.NewString(),
		requester: uuid.NewString(),
		approver:  uuid.NewString(),
		latencies: map[string]time.Duration{},
	}
	cleanupTenant(t, r.db, e.tenantA)
	cleanupTenant(t, r.db, e.tenantB)
	t.Cleanup(func() {
		cleanupTenant(t, r.db, e.tenantA)
		cleanupTenant(t, r.db, e.tenantB)
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(log, r.db,
		vendor.NewStore(r.db), requisition.NewStore(r.db), approval.NewStore(r.db),
		purchaseorder.NewStore(r.db), goodsreceipt.NewStore(r.db), invoice.NewStore(r.db),
		match.NewStore(r.db), dashboard.NewStore(r.db))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(context.Background(), lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = lis.Close()
	})
	e.client = procurementv1.NewProcurementClient(conn)
	return e
}

// asTenant mimics the gateway's propagation: user, tenant, and roles as
// outgoing gRPC metadata.
func (e *p2pEnv) asTenant(tenantID, userID string, roles ...string) context.Context {
	md := metadata.Pairs(rbac.MDUserID, userID, rbac.MDTenantID, tenantID)
	for _, r := range roles {
		md.Append(rbac.MDRole, r)
	}
	return metadata.NewOutgoingContext(context.Background(), md)
}

// as is asTenant on the story tenant.
func (e *p2pEnv) as(userID string, roles ...string) context.Context {
	return e.asTenant(e.tenantA, userID, roles...)
}

// ---------------------------------------------------------------------------
// MongoDB projection reads (always tenant-filtered — the AGENTS tenancy
// convention applies to test reads too).
// ---------------------------------------------------------------------------

// storedEvent is the projected event document shape the asserts need.
type storedEvent struct {
	EventID    string    `bson:"event_id"`
	Type       string    `bson:"type"`
	OccurredAt time.Time `bson:"occurred_at"`
	EntityType string    `bson:"entity_type"`
	EntityID   string    `bson:"entity_id"`
	ActorID    string    `bson:"actor_id"`
}

// storedAudit is the projected audit_log row shape the asserts need.
type storedAudit struct {
	EventID    string    `bson:"event_id"`
	Action     string    `bson:"action"`
	OccurredAt time.Time `bson:"occurred_at"`
	ActorID    string    `bson:"actor_id"`
	EntityType string    `bson:"entity_type"`
}

// events returns the tenant's projected events ordered by (occurred_at,
// event_id) ascending — the exact history.List sort, mirrored — which is a
// total order except across equal occurred_at values (the approval engine's
// step + terminal pair; see assertEventSequence).
// eventCount is events()' counting twin for cross-tenant assertions: a
// tenant that must own nothing (B in the cross-tenant probes) is asserted
// as zero on the Mongo projection itself, not only via the Postgres rows.
func (e *p2pEnv) eventCount(tenantID string) int {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 5*time.Second)
	defer cancel()
	n, err := e.run.mongoDB.Collection(history.EventsCollectionName).CountDocuments(ctx,
		bson.M{"tenant_id": tenantID})
	if err != nil {
		e.t.Fatalf("count events: %v", err)
	}
	return int(n)
}

func (e *p2pEnv) events() []storedEvent {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 5*time.Second)
	defer cancel()
	cursor, err := e.run.mongoDB.Collection(history.EventsCollectionName).Find(ctx,
		bson.M{"tenant_id": e.tenantA},
		options.Find().SetSort(bson.D{
			{Key: "occurred_at", Value: 1}, {Key: "event_id", Value: 1},
		}))
	if err != nil {
		e.t.Fatalf("find events: %v", err)
	}
	defer cursor.Close(ctx)
	out := []storedEvent{}
	for cursor.Next(ctx) {
		var ev storedEvent
		if err := cursor.Decode(&ev); err != nil {
			e.t.Fatalf("decode event: %v", err)
		}
		out = append(out, ev)
	}
	if err := cursor.Err(); err != nil {
		e.t.Fatalf("cursor events: %v", err)
	}
	return out
}

// auditRows returns the tenant's audit rows in the same (occurred_at,
// event_id) order.
func (e *p2pEnv) auditRows() []storedAudit {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 5*time.Second)
	defer cancel()
	cursor, err := e.run.mongoDB.Collection(history.AuditCollectionName).Find(ctx,
		bson.M{"tenant_id": e.tenantA},
		options.Find().SetSort(bson.D{
			{Key: "occurred_at", Value: 1}, {Key: "event_id", Value: 1},
		}))
	if err != nil {
		e.t.Fatalf("find audit: %v", err)
	}
	defer cursor.Close(ctx)
	out := []storedAudit{}
	for cursor.Next(ctx) {
		var a storedAudit
		if err := cursor.Decode(&a); err != nil {
			e.t.Fatalf("decode audit: %v", err)
		}
		out = append(out, a)
	}
	if err := cursor.Err(); err != nil {
		e.t.Fatalf("cursor audit: %v", err)
	}
	return out
}

// awaitProjected polls until the tenant has at least total projected events
// (deadline-bounded), recording the mutation→projection latency for the
// FR-11 evidence table.
func (e *p2pEnv) awaitProjected(step string, total int, started time.Time) {
	e.t.Helper()
	deadline := time.Now().Add(waitDeadline)
	var have []storedEvent
	for time.Now().Before(deadline) {
		have = e.events()
		if len(have) >= total {
			e.latencies[step] = time.Since(started)
			return
		}
		time.Sleep(pollInterval)
	}
	types := make([]string, 0, len(have))
	for _, ev := range have {
		types = append(types, ev.Type)
	}
	e.t.Fatalf("step %s: %d of %d events projected before the deadline (have %v)",
		step, len(have), total, types)
}

// matchTieAware checks want against the (occurred_at, event_id)-sorted
// events: want must be consumed in order, and events sharing an occurred_at
// (Mongo millisecond precision) are unordered among themselves — the
// approval engine writes pr.step_approved and the terminal decision event
// with ONE DecidedAt (appendDecisionEventsTx), so a bare occurred_at sort
// orders that pair arbitrarily (~50% flakes if asserted strictly).
// Tie-group semantics mirror history.List's documented total order: strict
// between distinct timestamps, arbitrary inside a tie group. allowExtras
// picks subsequence mode (unexpected types tolerated) over exact mode.
func matchTieAware(events []storedEvent, want []string, allowExtras bool) error {
	w := 0
	for i := 0; i < len(events); {
		j := i
		for j < len(events) && events[j].OccurredAt.Equal(events[i].OccurredAt) {
			j++
		}
		group := map[string]int{}
		for _, ev := range events[i:j] {
			group[ev.Type]++
		}
		for w < len(want) && group[want[w]] > 0 {
			group[want[w]]--
			w++
		}
		if !allowExtras {
			for typ, n := range group {
				if n > 0 {
					return fmt.Errorf("unexpected event %q at %s", typ, events[i].OccurredAt.Format(time.RFC3339Nano))
				}
			}
		}
		i = j
	}
	if w < len(want) {
		return fmt.Errorf("missing events %v (consumed %d of %d)", want[w:], w, len(want))
	}
	return nil
}

// assertEventSequence asserts the tenant's projected event types equal want
// (exact mode — nothing extra, nothing missing, order as above).
func (e *p2pEnv) assertEventSequence(step string, want []string) {
	e.t.Helper()
	got := e.events()
	if len(got) != len(want) {
		types := make([]string, 0, len(got))
		for _, ev := range got {
			types = append(types, ev.Type)
		}
		e.t.Fatalf("%s: projected %d events, want exactly %d (%v)", step, len(got), len(want), types)
	}
	if err := matchTieAware(got, want, false); err != nil {
		types := make([]string, 0, len(got))
		for _, ev := range got {
			types = append(types, ev.Type)
		}
		e.t.Fatalf("%s: event sequence mismatch (%v): %v", step, types, err)
	}
}

// assertAuditSequence asserts the tenant's audit rows carry exactly the
// want actions, in order — the closed AuditAction set: only decision events
// project audit rows, so e.g. pr.submitted and po.acknowledged must NOT
// appear (documents/history/audit.go).
func (e *p2pEnv) assertAuditSequence(step string, wantActions []string) {
	e.t.Helper()
	rows := e.auditRows()
	if len(rows) != len(wantActions) {
		e.t.Fatalf("%s: %d audit rows, want exactly %d", step, len(rows), len(wantActions))
	}
	got := make([]storedEvent, 0, len(rows))
	for _, r := range rows {
		got = append(got, storedEvent{EventID: r.EventID, Type: r.Action, OccurredAt: r.OccurredAt})
	}
	actions := make([]string, 0, len(rows))
	for _, r := range rows {
		actions = append(actions, r.Action)
	}
	if err := matchTieAware(got, wantActions, false); err != nil {
		e.t.Fatalf("%s: audit actions mismatch (%v): %v", step, actions, err)
	}
}

// reportLatencies logs the FR-11 evidence table for the dev note.
func (e *p2pEnv) reportLatencies() {
	e.t.Helper()
	e.t.Logf("tenant %s projection latencies (FR-11 budget 2s per event):", e.tenantA)
	for _, step := range []string{
		"submit", "approve", "convert", "issue", "acknowledge", "grn", "match",
	} {
		if d, ok := e.latencies[step]; ok {
			e.t.Logf("  %-12s %s", step, d)
		}
	}
}

// ---------------------------------------------------------------------------
// Direct PostgreSQL reads (state-consistency asserts bypass RLS on purpose:
// they are the test's privileged observer, not an application path).
// ---------------------------------------------------------------------------

// dbInt runs one scalar int query through the bypass arm.
func (e *p2pEnv) dbInt(query string, args ...any) int {
	e.t.Helper()
	var n int
	err := tenant.WithBypass(context.Background(), e.run.db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), query, args...).Scan(&n)
	})
	if err != nil {
		e.t.Fatalf("db query %q: %v", query, err)
	}
	return n
}

// outboxTotal is the tenant's outbox row count — the authoritative
// "nothing else was written" assert (synchronous, unlike Mongo).
func (e *p2pEnv) outboxTotal() int {
	return e.dbInt(`SELECT count(*) FROM procurement.outbox WHERE tenant_id = $1`, e.tenantA)
}

// outboxUnpublished is the tenant's pending row count — the publisher-loop
// proof at the end of the happy path.
func (e *p2pEnv) outboxUnpublished() int {
	return e.dbInt(`SELECT count(*) FROM procurement.outbox WHERE tenant_id = $1 AND published_at IS NULL`, e.tenantA)
}

// grnTotal is the tenant's goods-receipt count.
func (e *p2pEnv) grnTotal() int {
	return e.dbInt(`SELECT count(*) FROM procurement.goods_receipts WHERE tenant_id = $1`, e.tenantA)
}

// poTotal is the tenant's purchase-order count.
func (e *p2pEnv) poTotal() int {
	return e.dbInt(`SELECT count(*) FROM procurement.purchase_orders WHERE tenant_id = $1`, e.tenantA)
}
