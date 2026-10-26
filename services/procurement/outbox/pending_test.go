package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// The publisher reads across tenants by design; two scratch tenants prove it.
const (
	tenantA = "eeeeeeee-0000-0000-0000-000000000001"
	tenantB = "eeeeeeee-0000-0000-0000-000000000002"
)

func cleanupPending(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.outbox WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
		return err
	})
}

// appendEvent commits one event through the real writer path (a tenant-scoped
// transaction, exactly how Decide/Submit call AppendTx).
func appendEvent(t *testing.T, db *pgxpool.Pool, ev Event) {
	t.Helper()
	if err := tenant.WithTenant(context.Background(), db, ev.TenantID, func(tx pgx.Tx) error {
		return AppendTx(context.Background(), tx, ev)
	}); err != nil {
		t.Fatalf("append %s: %v", ev.EventType, err)
	}
}

func testEvent(id, eventType, tenantID string) Event {
	return Event{
		ID: id, TenantID: tenantID, EventType: eventType,
		Payload:    map[string]string{"number": "PR-000001"},
		EntityType: "requisition", EntityID: "eeeeeeee-0000-0000-0000-0000000000cc",
		OccurredAt: time.Now().UTC(),
	}
}

func publishedAt(t *testing.T, db *pgxpool.Pool, id string) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT published_at FROM procurement.outbox WHERE id = $1`, id).Scan(&ts)
	}); err != nil {
		t.Fatalf("read published_at: %v", err)
	}
	return ts
}

// pendingIDs returns the ids of the current pending batch. Pending reads
// every tenant's rows, and the DB-backed suites run in parallel against one
// test database — assertions must therefore match specific ids, not global
// counts.
func pendingIDs(t *testing.T, db *pgxpool.Pool, limit int) []string {
	t.Helper()
	rows, err := Pending(context.Background(), db, limit)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func contains(ids []string, want string) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}

// AC (crash between commit and publish loses nothing): a row committed via
// AppendTx but never marked — the crash simulation — is still returned by
// the next Pending poll, again and again until MarkPublished runs.
func TestPendingSurvivesCrashBeforePublish(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { cleanupPending(t, db) })
	cleanupPending(t, db)

	id := "eeeeeeee-0000-0000-0000-0000000000f1"
	appendEvent(t, db, testEvent(id, "pr.submitted", tenantA))

	for poll := 1; poll <= 2; poll++ {
		if idx := contains(pendingIDs(t, db, 50), id); idx < 0 {
			t.Fatalf("poll %d: the unmarked %s row is not pending", poll, id)
		}
	}
	if ts := publishedAt(t, db, id); ts != nil {
		t.Fatalf("published_at = %v before any marking, want NULL", ts)
	}

	// After marking, the row leaves the pending set but is never deleted.
	if err := MarkPublished(context.Background(), db, []string{id}); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if idx := contains(pendingIDs(t, db, 50), id); idx >= 0 {
		t.Fatalf("row %s still pending after marking", id)
	}
	first := publishedAt(t, db, id)
	if first == nil {
		t.Fatal("published_at not set after marking")
	}

	// Re-marking is a guarded no-op: the first timestamp survives.
	time.Sleep(2 * time.Millisecond) // let now() move past `first`
	if err := MarkPublished(context.Background(), db, []string{id}); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if again := publishedAt(t, db, id); !again.Equal(*first) {
		t.Fatalf("re-mark moved published_at: %v -> %v, want the first timestamp kept", first, again)
	}
}

// AC (one batch drains rows from ≥2 tenants): Pending is the publisher's
// deliberate cross-tenant read — rows from multiple tenants come back in one
// batch, oldest first — while the row-level security policy still hides one
// tenant's rows from another tenant's scoped read: the bypass arm (not a
// broken policy) is what spans tenants.
func TestPendingCrossTenantBatch(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { cleanupPending(t, db) })
	cleanupPending(t, db)

	a := testEvent("eeeeeeee-0000-0000-0000-0000000000fa", "pr.submitted", tenantA)
	b := testEvent("eeeeeeee-0000-0000-0000-0000000000fb", "pr.step_approved", tenantB)
	appendEvent(t, db, a)
	appendEvent(t, db, b)

	// One call returns both tenants' rows, in commit order (other tenants'
	// rows from parallel suites may ride along; the relative order of a and
	// b is what the publisher's oldest-first drain guarantees).
	ids := pendingIDs(t, db, 50)
	ia, ib := contains(ids, a.ID), contains(ids, b.ID)
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("one batch = %v, want %s (tenant A) before %s (tenant B)", ids, a.ID, b.ID)
	}
	rows, err := Pending(context.Background(), db, 50)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	var loaded *PendingEvent
	for i := range rows {
		if rows[i].ID == a.ID {
			loaded = &rows[i]
		}
	}
	if loaded == nil || len(loaded.Payload) == 0 || loaded.TenantID != tenantA || loaded.EventType != "pr.submitted" {
		t.Fatalf("tenant A row not fully loaded: %+v", loaded)
	}

	// The writer-side rule still holds: under tenant A's scoped read,
	// tenant B's row is invisible — RLS binds everything the bypass sees.
	var visibleB int
	if err := tenant.WithTenant(context.Background(), db, tenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.outbox WHERE id = $1`, b.ID).Scan(&visibleB)
	}); err != nil || visibleB != 0 {
		t.Fatalf("tenant-scoped read sees tenant B's row (%d, %v) — RLS must hide it", visibleB, err)
	}
	var visibleBviaBypass int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.outbox WHERE id = $1`, b.ID).Scan(&visibleBviaBypass)
	}); err != nil || visibleBviaBypass != 1 {
		t.Fatalf("bypass read misses tenant B's row (%d, %v), want 1", visibleBviaBypass, err)
	}

	// The batch limit bounds one poll.
	if got := len(pendingIDs(t, db, 1)); got != 1 {
		t.Fatalf("limited batch = %d rows, want 1", got)
	}
}

// Failure path: a non-positive limit fails fast without a database round
// trip. (The poll-error tolerance of the loop itself — e.g. the outbox table
// not existing yet — is exercised against a live database in the worker
// publisher suite.)
func TestPendingLimitGuard(t *testing.T) {
	db := openTestDB(t)
	if _, err := Pending(context.Background(), db, 0); err == nil {
		t.Fatal("Pending(0) succeeded, want error")
	}
}
