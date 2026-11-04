package requisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
)

const (
	tenantA = "eeeeeeee-0000-0000-0000-000000000001"
	tenantB = "eeeeeeee-0000-0000-0000-000000000002"
	tenantC = "eeeeeeee-0000-0000-0000-000000000003"
	userA   = "eeeeeeee-0000-0000-0000-0000000000aa"
)

// openTestDB mirrors the vendor harness: apply migrations, open a pool, skip
// without TEST_DATABASE_URL.
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

func testStoreDB(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	db := openTestDB(t)
	return NewStore(db), db
}

// num reads the numeric part of a PR-%06d number (-1 when unparseable).
func num(number string) int {
	var v int
	if _, err := fmt.Sscanf(number, "PR-%d", &v); err != nil {
		return -1
	}
	return v
}

func cleanupRequisition(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		// The outbox has no FK to requisitions; its rows must be removed
		// explicitly (the pr.submitted events of #45).
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.outbox WHERE tenant_id IN ($1, $2, $3)`, tenantA, tenantB, tenantC); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisitions WHERE tenant_id IN ($1, $2, $3) AND number LIKE 'PR-%'`,
			tenantA, tenantB, tenantC); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisition_counters WHERE tenant_id IN ($1, $2, $3)`, tenantA, tenantB, tenantC)
		return err
	})
}

var basketItems = []Item{
	{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
	{Description: "Mouse", Qty: 3, UnitPriceCents: 150000, Category: "it"},
}

// AC: drafts get a server-generated per-tenant number (PR-%06d); the counter
// lazy-initializes from the tenant's highest existing number; counters are
// independent per tenant.
func TestStoreCreateNumbering(t *testing.T) {
	s, db := testStoreDB(t)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	cleanupRequisition(t, db)
	ctx := context.Background()

	first, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.Number != "PR-000001" || first.Status != StatusDraft || first.TotalCents != 0 {
		t.Fatalf("first = %+v", first)
	}
	if first.RequesterID != userA || first.CreatedBy != userA || first.Currency != "IDR" {
		t.Fatalf("requester/audit/currency wrong: %+v", first)
	}

	withItems, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
	if err != nil {
		t.Fatalf("create with items: %v", err)
	}
	if withItems.Number != "PR-000002" {
		t.Fatalf("number = %s, want PR-000002", withItems.Number)
	}
	if withItems.TotalCents != 15450000 || len(withItems.Items) != 2 {
		t.Fatalf("total/items = %d/%d, want 15450000/2", withItems.TotalCents, len(withItems.Items))
	}

	// Tenant B has its own counter starting at 1.
	other, err := s.Create(ctx, tenantB, CreateInput{RequesterID: userA, Currency: "USD", Items: basketItems})
	if err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
	if other.Number != "PR-000001" || other.Currency != "USD" {
		t.Fatalf("tenant B = %s/%s, want PR-000001/USD", other.Number, other.Currency)
	}

	// Lazy init: a fresh tenant holding a pre-counter row (numbered
	// PR-000009 directly) gets its counter seeded from that row — the next
	// allocation is PR-000010, so fresh counters never collide with existing
	// data. (Once a counter exists it is authoritative; later direct rows do
	// not move it.)
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO procurement.requisitions (tenant_id, number, requester_id, currency)
			VALUES ($1, 'PR-000009', $2, 'IDR')`, tenantC, userA)
		return err
	}); err != nil {
		t.Fatalf("seed pre-counter row: %v", err)
	}
	lazy, err := s.Create(ctx, tenantC, CreateInput{RequesterID: userA, Currency: "IDR"})
	if err != nil {
		t.Fatalf("create after lazy init: %v", err)
	}
	if lazy.Number != "PR-000010" {
		t.Fatalf("lazy number = %s, want PR-000010", lazy.Number)
	}

	// Validation failure leaves no number gap: the counter increments inside
	// the same transaction as the insert and rolls back with it.
	before, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR"})
	if err != nil {
		t.Fatalf("create before invalid: %v", err)
	}
	if _, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "EUR"}); !errors.Is(err, ErrCurrencyInvalid) {
		t.Fatalf("invalid currency err = %v, want ErrCurrencyInvalid", err)
	}
	after, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR"})
	if err != nil {
		t.Fatalf("create after invalid: %v", err)
	}
	if after.Number != fmt.Sprintf("PR-%06d", num(before.Number)+1) {
		t.Fatalf("gap after rolled-back create: %s then %s", before.Number, after.Number)
	}
}

// AC (race-free numbering): concurrent creates in one tenant serialize on the
// counter row — every goroutine succeeds and the numbers form one
// gap-free consecutive run.
func TestStoreCreateConcurrentNumbers(t *testing.T) {
	s, db := testStoreDB(t)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	cleanupRequisition(t, db)

	const n = 8
	nums := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Create(context.Background(), tenantA,
				CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
			nums[i], errs[i] = r.Number, err
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	min, max := 1<<30, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent create %d: %v", i, err)
		}
		v := num(nums[i])
		if v <= 0 {
			t.Fatalf("bad number %q", nums[i])
		}
		if seen[v] {
			t.Fatalf("duplicate number %s", nums[i])
		}
		seen[v] = true
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if len(seen) != n || max-min+1 != n {
		t.Fatalf("numbers not one consecutive run: %v", nums)
	}
}

// AC: edit replaces the basket and recomputes the total; drafts only.
func TestStoreUpdateItems(t *testing.T) {
	s, db := testStoreDB(t)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	cleanupRequisition(t, db)
	ctx := context.Background()

	r, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := s.UpdateItems(ctx, tenantA, r.ID, basketItems[:1], userA)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.TotalCents != 15000000 || len(updated.Items) != 1 || updated.Status != StatusDraft {
		t.Fatalf("updated = %+v", updated)
	}
	if updated.UpdatedBy != userA {
		t.Fatalf("updated_by = %s, want %s", updated.UpdatedBy, userA)
	}

	// Validation errors carry the domain sentinel bare.
	if _, err := s.UpdateItems(ctx, tenantA, r.ID, []Item{{Description: "x", Qty: 0}}, userA); !errors.Is(err, ErrQtyInvalid) {
		t.Fatalf("invalid item err = %v, want ErrQtyInvalid", err)
	}

	// Empty basket is legal on a draft (lines are collected before submit).
	emptied, err := s.UpdateItems(ctx, tenantA, r.ID, nil, userA)
	if err != nil || len(emptied.Items) != 0 || emptied.TotalCents != 0 {
		t.Fatalf("empty edit = (%+v, %v)", emptied, err)
	}

	// AC: cross-tenant denial — tenant B cannot see or mutate tenant A's
	// requisition; it is indistinguishable from missing.
	for name, call := range map[string]func() error{
		"get":    func() error { _, err := s.Get(ctx, tenantB, r.ID); return err },
		"update": func() error { _, err := s.UpdateItems(ctx, tenantB, r.ID, basketItems, userA); return err },
		"submit": func() error { _, err := s.Submit(ctx, tenantB, r.ID, userA, "", nil); return err },
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant %s: err = %v, want ErrNotFound", name, err)
		}
	}

	// Bad/unknown ids fail fast or read as missing.
	if _, err := s.Get(ctx, tenantA, "not-a-uuid"); !errors.Is(err, ErrIDInvalid) {
		t.Fatalf("bad id err = %v, want ErrIDInvalid", err)
	}
	if _, err := s.Get(ctx, tenantA, "eeeeeeee-0000-0000-0000-0000000000cc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
}

// AC: submit only from draft, with an optional Idempotency-Key — replay
// returns the current state, key reuse across requisitions conflicts.
func TestStoreSubmitIdempotency(t *testing.T) {
	s, db := testStoreDB(t)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	cleanupRequisition(t, db)
	ctx := context.Background()

	a, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	empty, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR"})
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}

	// Submit needs at least one line.
	if _, err := s.Submit(ctx, tenantA, empty.ID, userA, "", nil); !errors.Is(err, ErrItemsEmpty) {
		t.Fatalf("empty submit err = %v, want ErrItemsEmpty", err)
	}

	// Submit with a key; replay of the same key returns the same submitted
	// state without error (and without a second transition).
	submitted, err := s.Submit(ctx, tenantA, a.ID, userA, "key-29-a", nil)
	if err != nil || submitted.Status != StatusSubmitted || submitted.SubmittedAt == nil {
		t.Fatalf("submit = (%+v, %v)", submitted, err)
	}
	replayed, err := s.Submit(ctx, tenantA, a.ID, userA, "key-29-a", nil)
	if err != nil || replayed.Status != StatusSubmitted || replayed.ID != a.ID {
		t.Fatalf("replay = (%+v, %v)", replayed, err)
	}
	if !replayed.SubmittedAt.Equal(*submitted.SubmittedAt) {
		t.Fatalf("replay moved submitted_at: %v vs %v", replayed.SubmittedAt, submitted.SubmittedAt)
	}

	// The same key cannot name another requisition's submit in the tenant.
	if _, err := s.Submit(ctx, tenantA, b.ID, userA, "key-29-a", nil); !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("key reuse err = %v, want ErrIdempotencyKeyConflict", err)
	}

	// Submit without a key still works; a second submit and any edit of a
	// submitted requisition are illegal transitions.
	if _, err := s.Submit(ctx, tenantA, b.ID, userA, "", nil); err != nil {
		t.Fatalf("submit b: %v", err)
	}
	if _, err := s.Submit(ctx, tenantA, b.ID, userA, "key-29-b", nil); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("resubmit err = %v, want ErrIllegalTransition", err)
	}
	if _, err := s.UpdateItems(ctx, tenantA, b.ID, basketItems, userA); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("edit submitted err = %v, want ErrIllegalTransition", err)
	}
}

// AC (#30 rollback): a materializer failure inside the submit transaction
// rolls the whole submit back — the requisition stays a draft, no
// submit_idempotency_key is recorded (the same key then names the fresh
// submit once the materializer succeeds), and no approval steps leak.
func TestStoreSubmitMaterializerRollback(t *testing.T) {
	s, db := testStoreDB(t)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	cleanupRequisition(t, db)
	ctx := context.Background()

	r, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	boom := errors.New("approval_rules_missing")
	failing := func(tx pgx.Tx, r Requisition) error { return boom }
	if _, err := s.Submit(ctx, tenantA, r.ID, userA, "key-30-a", failing); !errors.Is(err, boom) {
		t.Fatalf("failing materializer err = %v, want the materializer error", err)
	}

	stored, err := s.Get(ctx, tenantA, r.ID)
	if err != nil || stored.Status != StatusDraft || stored.SubmittedAt != nil {
		t.Fatalf("after rollback = (%+v, %v), want untouched draft", stored, err)
	}
	var steps int
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM procurement.approval_steps WHERE requisition_id = $1`, r.ID).Scan(&steps)
	}); err != nil || steps != 0 {
		t.Fatalf("steps after rollback = (%d, %v), want none", steps, err)
	}
	var keyCount int
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM procurement.requisitions WHERE id = $1 AND submit_idempotency_key IS NOT NULL`, r.ID).Scan(&keyCount)
	}); err != nil || keyCount != 0 {
		t.Fatalf("idempotency key recorded after rollback = (%d, %v), want none", keyCount, err)
	}

	// The key was never recorded: the same key now names this fresh submit.
	submitted, err := s.Submit(ctx, tenantA, r.ID, userA, "key-30-a", nil)
	if err != nil || submitted.Status != StatusSubmitted {
		t.Fatalf("submit after rollback = (%+v, %v), want submitted", submitted, err)
	}
	// And its replay is idempotent.
	if replayed, err := s.Submit(ctx, tenantA, r.ID, userA, "key-30-a", nil); err != nil ||
		replayed.Status != StatusSubmitted || !replayed.SubmittedAt.Equal(*submitted.SubmittedAt) {
		t.Fatalf("replay = (%+v, %v), want the same submitted state", replayed, err)
	}
}

// submitOutboxRows reads the pr.submitted events of one requisition straight
// from the table (bypassing RLS on purpose — the count is the atomicity
// evidence; the payload is the pinned cross-issue contract).
func submitOutboxRows(t *testing.T, db *pgxpool.Pool, requisitionID string) []SubmitPayload {
	t.Helper()
	rows := []SubmitPayload{}
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		rs, err := tx.Query(context.Background(), `
			SELECT payload FROM procurement.outbox
			WHERE entity_id = $1 AND event_type = $2
			ORDER BY created_at`, requisitionID, OutboxSubmitted)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var p SubmitPayload
			if err := rs.Scan(&p); err != nil {
				return err
			}
			rows = append(rows, p)
		}
		return rs.Err()
	})
	return rows
}

// AC (#45 write-in-same-transaction): a successful submit writes exactly one
// pr.submitted outbox row in the same transaction — carrying the payload
// contract, the actor, and the propagated trace id, published_at NULL until
// the publisher marks it; an idempotent replay writes nothing; rollbacks
// (approval rules missing, empty basket, illegal transition) write nothing.
func TestStoreSubmitOutboxEvent(t *testing.T) {
	s, db := testStoreDB(t)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	cleanupRequisition(t, db)
	// The trace id rides the context exactly as the TraceInterceptor puts
	// it there for gRPC calls.
	ctx := logger.WithTraceID(context.Background(), "trace-45-submit")

	// Rollback path first: a submit whose materializer fails (approval
	// rules missing) leaves no event, but the requisition stays a draft so
	// the retry below can succeed.
	r, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	boom := errors.New("approval_rules_missing")
	failing := func(tx pgx.Tx, r Requisition) error { return boom }
	if _, err := s.Submit(ctx, tenantA, r.ID, userA, "key-45-a", failing); !errors.Is(err, boom) {
		t.Fatalf("failing materializer err = %v, want the materializer error", err)
	}
	if got := len(submitOutboxRows(t, db, r.ID)); got != 0 {
		t.Fatalf("outbox rows after rollback = %d, want 0", got)
	}

	// Happy path: one row, full contract, committed with the transition.
	submitted, err := s.Submit(ctx, tenantA, r.ID, userA, "key-45-a", nil)
	if err != nil || submitted.Status != StatusSubmitted {
		t.Fatalf("submit = (%+v, %v)", submitted, err)
	}
	rows := submitOutboxRows(t, db, r.ID)
	if len(rows) != 1 {
		t.Fatalf("outbox rows after submit = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.RequisitionID != r.ID || got.Number != r.Number || got.RequesterID != userA ||
		got.Currency != "IDR" || got.TotalCents != 15450000 || got.SubmittedBy != userA ||
		!got.SubmittedAt.Equal(*submitted.SubmittedAt) {
		t.Fatalf("payload = %+v, want requisition %s / %s total 15450000 submitted at %v",
			got, r.ID, r.Number, submitted.SubmittedAt)
	}
	var actor, traceID string
	var published *time.Time
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT coalesce(actor_id::text, ''), trace_id, published_at
			FROM procurement.outbox WHERE entity_id = $1 AND event_type = $2`,
			r.ID, OutboxSubmitted).Scan(&actor, &traceID, &published)
	}); err != nil {
		t.Fatalf("load submit event row: %v", err)
	}
	if actor != userA || traceID != "trace-45-submit" || published != nil {
		t.Fatalf("row = (actor %s, trace %s, published %v), want (%s, trace-45-submit, NULL)",
			actor, traceID, published, userA)
	}

	// Idempotent replay: the current state returns, nothing is written.
	if _, err := s.Submit(ctx, tenantA, r.ID, userA, "key-45-a", nil); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := len(submitOutboxRows(t, db, r.ID)); got != 1 {
		t.Fatalf("outbox rows after replay = %d, want 1", got)
	}

	// A resubmit without the key is an illegal transition — no second event.
	if _, err := s.Submit(ctx, tenantA, r.ID, userA, "", nil); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("resubmit err = %v, want ErrIllegalTransition", err)
	}
	if got := len(submitOutboxRows(t, db, r.ID)); got != 1 {
		t.Fatalf("outbox rows after resubmit = %d, want 1", got)
	}

	// An empty basket fails domain validation before any write.
	empty, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR"})
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	if _, err := s.Submit(ctx, tenantA, empty.ID, userA, "", nil); !errors.Is(err, ErrItemsEmpty) {
		t.Fatalf("empty submit err = %v, want ErrItemsEmpty", err)
	}
	if got := len(submitOutboxRows(t, db, empty.ID)); got != 0 {
		t.Fatalf("empty requisition outbox rows = %d, want 0", got)
	}
}
