package purchaseorder

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// Scale fixture ids isolate the 100k-row fixture from every other test's
// data (the eeeeeeee family is this package family's test-tenant prefix; the
// #32 requisition scale test uses the 0032 slot).
const (
	scaleTenant = "eeeeeeee-0038-0000-0000-000000000038"
	scaleUser   = "eeeeeeee-0038-0000-0000-0000000000aa"
	scaleVendor = "eeeeeeee-0038-0000-0000-0000000000bb"
)

// listBudgetMs is the per-query budget the list must meet at 100k rows — the
// PRD §19 bar (p95 < 300 ms for list/detail endpoints on a 100k-item seed),
// same number as the vendor (#27) and requisition (#32) lists.
const listBudgetMs = 300

// scaleLockKey is the advisory-lock key shared by the three 100k budget
// fixtures (vendor #27, requisition #32, purchaseorder #38). `go test ./...`
// runs their packages concurrently against one database, and their combined
// bulk-load/IO spikes pushed neighbouring timed loops over the p95 budget
// (CI run 40797112295: the vendor unfiltered shape measured p95 322 ms —
// median 209 ms vs ~73 ms solo — while all three fixtures bulk-loaded at
// once). Every heavy unit (a seed batch, the VACUUM/CHECKPOINT hygiene, one
// measurement shape) takes and releases the lock, so at most one such unit
// runs at a time without serializing whole tests — a whole-test lock would
// exceed the go test timeout, since the solo fixtures run 150–450 s.
const scaleLockKey = 9138271001

// scaleGuard holds the one dedicated connection advisory locks live on
// (pool connections rotate per query; advisory locks are session-scoped).
type scaleGuard struct {
	conn *pgxpool.Conn
}

func newScaleGuard(t *testing.T, db *pgxpool.Pool) *scaleGuard {
	t.Helper()
	conn, err := db.Acquire(context.Background())
	if err != nil {
		t.Fatalf("scale guard connection: %v", err)
	}
	t.Cleanup(conn.Release)
	return &scaleGuard{conn: conn}
}

// lock takes the shared fixture lock; call unlock when the heavy unit ends
// (defer right after).
func (g *scaleGuard) lock(t *testing.T) {
	t.Helper()
	if _, err := g.conn.Exec(context.Background(), `SELECT pg_advisory_lock($1)`, scaleLockKey); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}
}

func (g *scaleGuard) unlock() {
	_, _ = g.conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, scaleLockKey)
}

// TestStoreListScale100k proves the #38 list stays under the PRD §19 budget
// at 100k purchase orders in one tenant, across the four filter shapes the
// #27/#32 conventions pin (unfiltered / status / number prefix / combined).
// The EXPLAIN (ANALYZE, BUFFERS) plans are logged and mirrored in
// docs/schema/procurement.md.
//
// Unlike the requisition fixture (#32, no FK dependencies), purchase_orders
// carries two foreign keys plus a unique constraint: requisition_id →
// requisitions with UNIQUE (tenant_id, requisition_id) — one PO per
// requisition — and vendor_id → vendors. The fixture therefore seeds 100k
// requisitions (deterministic ids, one per PO) and one vendor before the
// POs, and the cleanup deletes the POs BEFORE the requisitions (plus both
// counters and the vendor).
func TestStoreListScale100k(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)
	guard := newScaleGuard(t, db)

	cleanupScale(t, s)
	if err := seedScale(t, s, guard, 100_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { cleanupScale(t, s) })

	// Production steady state before measuring (#32 fixture hygiene):
	// VACUUM (ANALYZE) sets the visibility map and statistics after a bulk
	// load (without it the count arm's index-only scan degrades to heap
	// fetches), and CHECKPOINT flushes the fixture's dirty buffers so the
	// background checkpointer does not compete with the timing loop. Both
	// need superuser, so they run on the pool directly — and under the
	// shared fixture lock (see scaleLockKey): a global CHECKPOINT flushes
	// every database's buffers at once.
	for _, table := range []string{"procurement.purchase_orders"} {
		guard.lock(t)
		_, err := s.db.Exec(t.Context(), fmt.Sprintf(`VACUUM (ANALYZE) %s`, table))
		guard.unlock()
		if err != nil {
			t.Fatalf("vacuum analyze %s: %v", table, err)
		}
	}
	guard.lock(t)
	_, err := s.db.Exec(t.Context(), `CHECKPOINT`)
	guard.unlock()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	shapes := []struct {
		name string
		f    ListFilter
	}{
		{"unfiltered page 1", ListFilter{}},
		{"status filter", ListFilter{Status: StatusIssued}}, // ~10% of rows
		{"number prefix", ListFilter{Query: "PO-3809"}},     // ~1k of the 100k numbers
		{"combined status+query", ListFilter{Query: "PO-38", Status: StatusDraft}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			f, err := ValidateList(shape.f)
			if err != nil {
				t.Fatalf("ValidateList: %v", err)
			}
			// The timed loop is a heavy unit too: hold the shared fixture
			// lock so no other package's seed batch or CHECKPOINT can spike
			// the middle of the budget measurement.
			guard.lock(t)
			defer guard.unlock()
			// Warm-ups (fill caches; the budget is steady-state).
			for range 3 {
				if _, _, err := s.List(t.Context(), scaleTenant, f); err != nil {
					t.Fatalf("list: %v", err)
				}
			}
			const runs = 30
			durations := make([]time.Duration, 0, runs)
			for i := 0; i < runs; i++ {
				start := time.Now()
				rows, total, err := s.List(t.Context(), scaleTenant, f)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				durations = append(durations, time.Since(start))
				if len(rows) == 0 || total == 0 {
					t.Fatalf("shape %s returned nothing (rows=%d total=%d)", shape.name, len(rows), total)
				}
			}
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			median := durations[len(durations)/2]
			p95 := durations[(len(durations)*95)/100]
			t.Logf("%s: median=%v p95=%v over %d runs (budget %dms)", shape.name, median, p95, len(durations), listBudgetMs)
			if p95.Milliseconds() >= listBudgetMs {
				t.Fatalf("%s: p95=%v exceeds the %dms budget", shape.name, p95, listBudgetMs)
			}
			explainList(t, s, f)
		})
	}
}

// scaleReqID builds the deterministic requisition id of fixture row i; the
// PO rows reference these ids, and every group stays valid hex.
func scaleReqID(i int) string {
	return fmt.Sprintf("eeeeee38-0000-0000-%04d-%012d", i%10000, i)
}

// seedScale bulk-loads one vendor, n requisitions, and n purchase orders for
// scaleTenant via batched multi-row INSERTs under the RLS bypass context
// (COPY FROM is rejected on RLS-enabled tables regardless of the bypass
// setting — the #27/#32 finding). PO numbers use the PO-38xxxxx space so a
// short prefix selects a known ~1k slice; statuses split 80% draft / 10%
// issued / 10% acknowledged. Each PO names its own requisition (the UNIQUE
// (tenant_id, requisition_id) "one PO per requisition" rule) and the shared
// scaleVendor. Batches commit one transaction each under the shared fixture
// lock (see scaleLockKey) so no batch's IO spike lands inside another
// package's budget measurement.
func seedScale(t *testing.T, s *Store, guard *scaleGuard, n int) error {
	t.Helper()
	const batchSize = 500
	if err := func() error {
		guard.lock(t)
		defer guard.unlock()
		return tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
			ctx := t.Context()
			if _, err := tx.Exec(ctx, `
				INSERT INTO procurement.vendors (id, tenant_id, code, name, status, created_by)
				VALUES ($1, $2, 'SCALE-38', 'Scale Vendor 38', 'active', $3)
				ON CONFLICT DO NOTHING`, scaleVendor, scaleTenant, scaleUser); err != nil {
				return fmt.Errorf("vendor: %w", err)
			}
			return nil
		})
	}(); err != nil {
		return err
	}
	for start := 1; start <= n; start += batchSize {
		end := min(start+batchSize-1, n)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*4)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.requisitions (id, tenant_id, number, requester_id, status, currency, total_cents) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			// $n = id, $n+1 = tenant, $n+2 = number, $n+3 = requester.
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,'closed','IDR',1000000)",
				rowNum*4-3, rowNum*4-2, rowNum*4-1, rowNum*4)
			args = append(args, scaleReqID(i), scaleTenant, fmt.Sprintf("PR-38%05d", i), scaleUser)
		}
		if err := commitBatch(t, s, guard, sb.String(), args); err != nil {
			return fmt.Errorf("requisitions %d-%d: %w", start, end, err)
		}
	}
	for start := 1; start <= n; start += batchSize {
		end := min(start+batchSize-1, n)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*3)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.purchase_orders (tenant_id, number, requisition_id, vendor_id, status, currency, total_cents) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			status := StatusDraft
			switch {
			case i%10 == 3:
				status = StatusAcknowledged
			case i%10 == 7:
				status = StatusIssued
			}
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			// $n = number, $n+1 = requisition_id, $n+2 = status; the
			// tenant and the shared vendor are constants.
			fmt.Fprintf(&sb, "('%s', $%d, $%d, '%s', $%d, 'IDR', 1000000)",
				scaleTenant, rowNum*3-2, rowNum*3-1, scaleVendor, rowNum*3)
			args = append(args, fmt.Sprintf("PO-38%05d", i), scaleReqID(i), status)
		}
		if err := commitBatch(t, s, guard, sb.String(), args); err != nil {
			return fmt.Errorf("purchase orders %d-%d: %w", start, end, err)
		}
	}
	return nil
}

// commitBatch runs one batched INSERT in its own bypass transaction under
// the shared fixture lock.
func commitBatch(t *testing.T, s *Store, guard *scaleGuard, query string, args []any) error {
	t.Helper()
	guard.lock(t)
	defer guard.unlock()
	return tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), query, args...)
		return err
	})
}

// cleanupScale removes the scale fixture: POs (their items cascade) BEFORE
// the requisitions they reference (items/steps cascade too), then both
// counters and the vendor — FK-safe order.
func cleanupScale(t *testing.T, s *Store) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), s.db, func(tx pgx.Tx) error {
		ctx := context.Background()
		for _, stmt := range []string{
			`DELETE FROM procurement.purchase_orders WHERE tenant_id = $1`,
			`DELETE FROM procurement.po_counters WHERE tenant_id = $1`,
			`DELETE FROM procurement.requisitions WHERE tenant_id = $1`,
			`DELETE FROM procurement.requisition_counters WHERE tenant_id = $1`,
			`DELETE FROM procurement.vendors WHERE tenant_id = $1 AND code = 'SCALE-38'`,
		} {
			if _, err := tx.Exec(ctx, stmt, scaleTenant); err != nil {
				return err
			}
		}
		return nil
	})
	// Reclaim the fixture's dead tuples so a re-run of the suite measures
	// tables of live rows, not live + the previous run's dead ones.
	_, _ = s.db.Exec(context.Background(), `VACUUM ANALYZE procurement.purchase_orders`)
	_, _ = s.db.Exec(context.Background(), `VACUUM ANALYZE procurement.requisitions`)
}

// explainList logs the EXPLAIN (ANALYZE, BUFFERS) plan of the list query
// under the real RLS tenant context — the evidence mirrored in the schema
// docs.
func explainList(t *testing.T, s *Store, f ListFilter) {
	t.Helper()
	offset := (int(f.Page) - 1) * int(f.PageSize)
	var plan strings.Builder
	err := tenant.WithTenant(t.Context(), s.db, scaleTenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(t.Context(), `
			EXPLAIN (ANALYZE, BUFFERS)
			SELECT c.id, c.tenant_id, c.number, c.requisition_id, c.vendor_id, c.status, c.currency,
			       c.total_cents, c.issued_at, c.created_at, c.created_by, c.updated_at, c.updated_by,
			       (SELECT count(*) FROM procurement.purchase_orders `+listPredicates+`) AS total_count
			FROM (SELECT id, tenant_id, number, requisition_id, vendor_id, status, currency,
			             total_cents, issued_at, created_at, created_by, updated_at, updated_by
			      FROM procurement.purchase_orders
			      `+listPredicates+`
			      ORDER BY number COLLATE "C" DESC
			      LIMIT $3 OFFSET $4) c
			ORDER BY c.number DESC`,
			f.Status, f.Query, f.PageSize, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan.WriteString(line)
			plan.WriteByte('\n')
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	t.Logf("EXPLAIN (ANALYZE, BUFFERS) status=%q query=%q:\n%s", f.Status, f.Query, plan.String())
}
