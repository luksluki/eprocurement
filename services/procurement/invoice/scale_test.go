package invoice

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/scalegate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// Scale fixture ids isolate the ~100k-row fixture from every other test's
// data (the eeeeeeee family is this package family's test-tenant prefix;
// this package takes the 0056 slot, after dashboard's 0052).
const (
	scaleTenant = "eeeeeeee-0056-0000-0000-000000000056"
	scaleUser   = "eeeeeeee-0056-0000-0000-0000000000aa"
)

// listBudgetMs is the per-query budget the list must meet at ~100k rows —
// the PRD's p95 < 300 ms performance bar (NFR §19 list row; the vendor #27 /
// requisition #32 / purchase-order #38 lists are held to the same number;
// the #41 measured-plan section deferred exactly this fixture to #56).
const listBudgetMs = 300

// shapeAttempts bounds the budget's retries. The budget is a steady-state
// p95 claim, but CI runs `go test ./...` with the 100k budget fixtures
// (vendor #27, requisition #32, purchase order #38, dashboard #52, this one)
// loading concurrently against one database, and a neighbour's bulk-load
// burst or CHECKPOINT can spike a timed loop — the #38 protocol
// (dev-notes 038/041, maintainer-approved for #52): warm-ups + 20 timed
// iterations, a bust retries after a cooldown, the final attempt decisive.
const (
	shapeAttempts   = 3
	shapeRetryPause = 5 * time.Second
)

// scaleVendorID builds the deterministic vendor id of fixture slot v
// (16 vendors, mirroring the #52 dashboard fixture's shape).
func scaleVendorID(v int) string {
	return fmt.Sprintf("eeeeee56-0000-0003-0000-0000000000%02d", v)
}

// scaleReqID / scalePOID / scalePOItemID / scaleInvoiceID build the
// deterministic fixture ids; every group stays valid hex. Invoice i bills
// PO ((i-1) % 10_000) + 1 — ten invoices per PO, so the per-PO aggregates
// the create path maintains stay realistic at invoice scale.
func scaleReqID(i int) string {
	return fmt.Sprintf("eeeeee56-0000-0000-%04d-%012d", i%10000, i)
}

func scalePOID(i int) string {
	return fmt.Sprintf("eeeeee56-0000-0001-%04d-%012d", i%10000, i)
}

func scalePOItemID(i, line int) string {
	return fmt.Sprintf("eeeeee56-0000-0002-%04d-%012d", i%10000, line)
}

func scaleInvoiceID(i int) string {
	return fmt.Sprintf("eeeeee56-0000-0004-%04d-%012d", i%10000, i)
}

// scalePrefixMatch reports whether invoice row i carries a number in the
// "INV-5609" prefix shape: with the INV-56%05d number space, that prefix
// selects exactly i in [9000, 9999] — ~1k of the 100k numbers, the same
// slice shape the requisition fixture's "PR-3209" query selects.
func scalePrefixMatch(i int) bool { return i >= 9000 && i <= 9999 }

// scaleFixture is the deterministic shape the seedScale write loop tracks
// while it writes — every expected number is tracked, never re-derived from
// the store under test (the #52 reconciliation rule).
type scaleFixture struct {
	// Row counts per list shape, tracked at write time.
	unfiltered int
	status     int // status = matched
	prefix     int // number prefix "INV-5609"
	combined   int // number prefix "INV-5609" AND status = discrepancy

	// The page-1 first row per shape (created_at DESC, id DESC — created_at
	// is monotonic in the row index, so the newest row is the highest i).
	firstUnfiltered string
	firstStatus     string
	firstPrefix     string
	firstCombined   string
}

// TestStoreListScale100k proves the #56 acceptance criteria on one
// ~100k-row invoice fixture in the procurement.* schema (the #41 list
// gained its indexes in 0012 but had no scale fixture — this closes it on
// the #27/#32/#38 pattern):
//
//  1. Reconciliation — every list shape's total equals the count tracked
//     while writing and the page-1 first row matches the tracked newest row.
//  2. Performance — every filter shape (unfiltered page 1, status, number
//     prefix, combined) stays under the 300 ms p95 budget across 20 timed
//     iterations after warm-ups.
//  3. Plans — the EXPLAIN (ANALYZE, BUFFERS) of every shape is logged for
//     the dev note and docs/perf.md (the dev-note #41 evidence protocol).
func TestStoreListScale100k(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)

	// Serialize the ~100k fixtures' LOAD phase (see
	// internal/platform/scalegate): concurrent bulk loads starved every one
	// past go test's package timeout (CI run 41198373932). The gate is
	// released after the steady-state prep so the short, retry-protected
	// measurement loops overlap instead of queueing. The t.Cleanup release
	// is the early-exit safety net; the explicit release below is the real
	// one.
	releaseGate, err := scalegate.Acquire(t.Context(), db)
	if err != nil {
		t.Fatalf("scale gate: %v", err)
	}
	t.Cleanup(releaseGate)

	cleanupScale(t, db)
	fixture, err := seedScale(t, db, 100_000)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { cleanupScale(t, db) })

	// Production steady state before measuring (the #32/#38/#52 fixture
	// hygiene): VACUUM (ANALYZE) sets the visibility map and statistics
	// after a bulk load, CHECKPOINT flushes the fixture's dirty buffers so
	// the background checkpointer does not compete with the timing loop.
	for _, table := range []string{
		"procurement.invoices", "procurement.invoice_lines",
		"procurement.purchase_orders", "procurement.requisitions",
	} {
		if _, err := db.Exec(t.Context(), `VACUUM (ANALYZE) `+table); err != nil {
			t.Fatalf("vacuum analyze %s: %v", table, err)
		}
	}
	if _, err := db.Exec(t.Context(), `CHECKPOINT`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	releaseGate() // load phase done — measurements overlap under the retry protocol

	shapes := []struct {
		name        string
		f           ListFilter
		wantTotal   int
		wantFirstNo string
	}{
		{"unfiltered page 1", ListFilter{}, fixture.unfiltered, fixture.firstUnfiltered},
		{"status filter", ListFilter{Status: StatusMatched}, fixture.status, fixture.firstStatus},
		{"number prefix", ListFilter{Query: "INV-5609"}, fixture.prefix, fixture.firstPrefix},
		{"combined status+query", ListFilter{Query: "INV-5609", Status: StatusDiscrepancy}, fixture.combined, fixture.firstCombined},
	}

	t.Run("reconciles with the fixture", func(t *testing.T) {
		for _, shape := range shapes {
			f, err := ValidateList(shape.f)
			if err != nil {
				t.Fatalf("%s: ValidateList: %v", shape.name, err)
			}
			rows, total, err := s.List(t.Context(), scaleTenant, f)
			if err != nil {
				t.Fatalf("%s: list: %v", shape.name, err)
			}
			if total != shape.wantTotal {
				t.Fatalf("%s: total = %d, want the tracked %d", shape.name, total, shape.wantTotal)
			}
			if len(rows) == 0 {
				t.Fatalf("%s: page 1 is empty", shape.name)
			}
			if rows[0].Number != shape.wantFirstNo {
				t.Fatalf("%s: newest row = %s, want the tracked %s",
					shape.name, rows[0].Number, shape.wantFirstNo)
			}
			if len(rows[0].Items) != 0 || rows[0].MatchResult != "" {
				t.Fatalf("%s: list rows must be header-only (the detail read owns lines and match_result)", shape.name)
			}
		}
	})

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			f, err := ValidateList(shape.f)
			if err != nil {
				t.Fatalf("ValidateList: %v", err)
			}
			var p95 time.Duration
			for attempt := 1; attempt <= shapeAttempts; attempt++ {
				// Warm-ups (fill caches; the budget is steady-state).
				for range 3 {
					if _, _, err := s.List(t.Context(), scaleTenant, f); err != nil {
						t.Fatalf("list warm-up: %v", err)
					}
				}
				const runs = 20
				durations := make([]time.Duration, 0, runs)
				for i := 0; i < runs; i++ {
					start := time.Now()
					rows, total, err := s.List(t.Context(), scaleTenant, f)
					durations = append(durations, time.Since(start))
					if err != nil {
						t.Fatalf("list: %v", err)
					}
					if len(rows) == 0 || total != shape.wantTotal {
						t.Fatalf("shape %s iteration %d: rows=%d total=%d, want rows>0 total=%d",
							shape.name, i, len(rows), total, shape.wantTotal)
					}
				}
				sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
				median := durations[len(durations)/2]
				p95 = durations[(len(durations)*95)/100]
				t.Logf("%s: attempt %d/%d: median=%v p95=%v over %d runs (budget %dms)",
					shape.name, attempt, shapeAttempts, median, p95, len(durations), listBudgetMs)
				if p95.Milliseconds() < listBudgetMs {
					explainList(t, s, f)
					return
				}
				if attempt < shapeAttempts {
					t.Logf("%s: p95 over budget — likely fixture contention, retrying after %v",
						shape.name, shapeRetryPause)
					time.Sleep(shapeRetryPause)
				}
			}
			t.Fatalf("%s: p95=%v exceeds the %dms budget", shape.name, p95, listBudgetMs)
		})
	}
}

// seedScale bulk-loads the ~100k invoice fixture under the RLS bypass
// context via batched multi-row INSERTs (the #27/#32/#38/#52 harness; COPY
// FROM is not an option on RLS-enabled tables). Shape, all deterministic in
// the invoice row index i (1..n):
//
//   - invoices: number INV-56xxxxx; status i%10==3 → matched · 7 →
//     discrepancy · else pending (80/10/10); created_at now-(n-i)*30d/n —
//     monotonic in i, so the list's created_at DESC order is generation
//     order; currency follows the billing PO's (USD when i%7==0); one line
//     billing the PO's first item.
//   - POs: one per requisition (10k, i.e. ten invoices per PO), status
//     received, issued now-45d, vendor i%16 over the 16 fixture vendors.
//   - requisitions: the POs' conversion parents (closed; nothing under test
//     reads them beyond the FK).
//   - vendors: 16 (the #52 shape).
func seedScale(t *testing.T, db *pgxpool.Pool, n int) (*scaleFixture, error) {
	t.Helper()
	fixture := &scaleFixture{}
	now := time.Now().UTC()
	spacing := (30 * 24 * time.Hour) / time.Duration(n)

	// Vendors (ON CONFLICT DO NOTHING: a prior run's cleanup failure must
	// not wedge the seed).
	for v := 0; v < 16; v++ {
		if err := commitScaleBatch(t, db, `
			INSERT INTO procurement.vendors (id, tenant_id, code, name, status, created_by)
			VALUES ($1, $2, $3, $4, 'active', $5) ON CONFLICT DO NOTHING`,
			[]any{scaleVendorID(v), scaleTenant, fmt.Sprintf("SCALE-56-%02d", v),
				fmt.Sprintf("Scale Vendor 56-%02d", v), scaleUser}); err != nil {
			return nil, fmt.Errorf("vendors: %w", err)
		}
	}

	const batchSize = 500
	// Requisitions: the POs' parents, one per PO slot.
	for start := 1; start <= 10_000; start += batchSize {
		end := min(start+batchSize-1, 10_000)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*3)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.requisitions (id, tenant_id, number, requester_id, status, currency, total_cents) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "($%d,'%s',$%d,$%d,'closed','IDR',1000000)", rowNum*3-2, scaleTenant, rowNum*3-1, rowNum*3)
			args = append(args, scaleReqID(i), fmt.Sprintf("PR-56%05d", i), scaleUser)
		}
		if err := commitScaleBatch(t, db, sb.String(), args); err != nil {
			return nil, fmt.Errorf("requisitions %d-%d: %w", start, end, err)
		}
	}

	// POs: one per requisition; the invoice's currency derives from it.
	for start := 1; start <= 10_000; start += batchSize {
		end := min(start+batchSize-1, 10_000)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*5)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.purchase_orders (id, tenant_id, number, requisition_id, vendor_id, status, currency, total_cents, issued_at, created_by) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			currency := "IDR"
			if i%7 == 0 {
				currency = "USD"
			}
			fmt.Fprintf(&sb, "($%d,'%s',$%d,$%d,$%d,'received','%s',1000000,now() - interval '45 days',$%d)",
				rowNum*5-4, scaleTenant, rowNum*5-3, rowNum*5-2, rowNum*5-1, currency, rowNum*5)
			args = append(args, scalePOID(i), fmt.Sprintf("PO-56%05d", i), scaleReqID(i),
				scaleVendorID(i%16), scaleUser)
		}
		if err := commitScaleBatch(t, db, sb.String(), args); err != nil {
			return nil, fmt.Errorf("purchase orders %d-%d: %w", start, end, err)
		}
	}

	// PO items: two per PO; invoice lines bill the first.
	for start := 1; start <= 10_000; start += batchSize {
		end := min(start+batchSize-1, 10_000)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*4)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.po_items (id, po_id, description, qty_ordered, unit_price_cents) VALUES `)
		for i := start; i <= end; i++ {
			for line := 0; line < 2; line++ {
				rowNum++
				if rowNum > 1 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, "($%d,$%d,'scale item',10,1000)", rowNum*2-1, rowNum*2)
				args = append(args, scalePOItemID(i, line), scalePOID(i))
			}
		}
		if err := commitScaleBatch(t, db, sb.String(), args); err != nil {
			return nil, fmt.Errorf("po items %d-%d: %w", start, end, err)
		}
	}

	// Invoices: the table under test, n rows.
	invoiceStatus := func(i int) string {
		switch i % 10 {
		case 3:
			return StatusMatched
		case 7:
			return StatusDiscrepancy
		default:
			return StatusPending
		}
	}
	for start := 1; start <= n; start += batchSize {
		end := min(start+batchSize-1, n)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*5)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.invoices (id, tenant_id, po_id, vendor_id, number, currency, total_cents, status, created_at, created_by) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			number := fmt.Sprintf("INV-56%05d", i)
			status := invoiceStatus(i)
			created := now.Add(-time.Duration(n-i) * spacing)
			currency := "IDR"
			if i%7 == 0 {
				currency = "USD"
			}
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "($%d,'%s',$%d,$%d,$%d,'%s',%d,'%s',$%d,'%s')",
				rowNum*5-4, scaleTenant, rowNum*5-3, rowNum*5-2, rowNum*5-1, currency,
				1_000_000+i, status, rowNum*5, scaleUser)
			args = append(args, scaleInvoiceID(i), scalePOID((i-1)%10_000+1),
				scaleVendorID(i%16), number, created)

			fixture.unfiltered++
			if status == StatusMatched {
				fixture.status++
			}
			if scalePrefixMatch(i) {
				fixture.prefix++
				if status == StatusDiscrepancy {
					fixture.combined++
				}
			}
			// created_at is monotonic in i, so the newest row of a shape is
			// the last match seen (i ascends).
			fixture.firstUnfiltered = number
			if status == StatusMatched {
				fixture.firstStatus = number
			}
			if scalePrefixMatch(i) {
				fixture.firstPrefix = number
				if status == StatusDiscrepancy {
					fixture.firstCombined = number
				}
			}
		}
		if err := commitScaleBatch(t, db, sb.String(), args); err != nil {
			return nil, fmt.Errorf("invoices %d-%d: %w", start, end, err)
		}
	}

	// Invoice lines: one per invoice, billing the PO's first item.
	for start := 1; start <= n; start += batchSize {
		end := min(start+batchSize-1, n)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*2)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.invoice_lines (invoice_id, po_item_id, qty, unit_price_cents) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "($%d,$%d,1,1000)", rowNum*2-1, rowNum*2)
			args = append(args, scaleInvoiceID(i), scalePOItemID((i-1)%10_000+1, 0))
		}
		if err := commitScaleBatch(t, db, sb.String(), args); err != nil {
			return nil, fmt.Errorf("invoice lines %d-%d: %w", start, end, err)
		}
	}
	return fixture, nil
}

// commitScaleBatch runs one batched INSERT in its own bypass transaction.
func commitScaleBatch(t *testing.T, db *pgxpool.Pool, query string, args []any) error {
	t.Helper()
	return tenant.WithBypass(t.Context(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), query, args...)
		return err
	})
}

// deleteScaleFixture removes the scale fixture's rows in FK-safe order
// (invoices — lines cascade — before the POs and requisitions they
// reference, then the vendors).
func deleteScaleFixture(tx pgx.Tx) error {
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM procurement.invoices WHERE tenant_id = $1`,
		`DELETE FROM procurement.purchase_orders WHERE tenant_id = $1`,
		`DELETE FROM procurement.requisitions WHERE tenant_id = $1`,
		`DELETE FROM procurement.vendors WHERE tenant_id = $1 AND code LIKE 'SCALE-56-%'`,
	} {
		if _, err := tx.Exec(ctx, stmt, scaleTenant); err != nil {
			return err
		}
	}
	return nil
}

// cleanupScale removes the scale fixture in FK-safe order and reclaims the
// dead tuples so a re-run measures live rows only.
func cleanupScale(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	// One retry: right after a ~200k-row bulk load the compose PostgreSQL
	// intermittently answers the big DELETE with "MultiXactId … has not been
	// created yet -- apparent wraparound" (XX000) — transient, a fresh
	// transaction seconds later succeeds (the #52 dashboard cleanup saw the
	// same). A silent failure here would leave the fixture behind and break
	// the next run on duplicate keys, so the retry is load-bearing.
	err := tenant.WithBypass(context.Background(), db, deleteScaleFixture)
	if err != nil {
		t.Logf("cleanupScale delete: %v — retrying once after a pause", err)
		time.Sleep(2 * time.Second)
		err = tenant.WithBypass(context.Background(), db, deleteScaleFixture)
	}
	if err != nil {
		t.Fatalf("cleanupScale delete failed twice: %v", err)
	}
	for _, table := range []string{
		"procurement.invoices", "procurement.purchase_orders", "procurement.requisitions",
	} {
		_, _ = db.Exec(context.Background(), `VACUUM ANALYZE `+table)
	}
}

// explainList logs the EXPLAIN (ANALYZE, BUFFERS) of the exact list
// statement under the real RLS tenant context — the evidence mirrored in
// docs/schema/procurement.md and docs/perf.md (the dev-note #41 protocol).
func explainList(t *testing.T, s *Store, f ListFilter) {
	t.Helper()
	offset := (int(f.Page) - 1) * int(f.PageSize)
	var plan strings.Builder
	err := tenant.WithTenant(t.Context(), s.db, scaleTenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(t.Context(), `
			EXPLAIN (ANALYZE, BUFFERS)
			SELECT c.id, c.tenant_id, c.po_id, c.vendor_id, c.number, c.currency,
			       c.total_cents, c.status, c.created_at, c.created_by,
			       (SELECT count(*) FROM procurement.invoices `+listPredicates+`) AS total_count
			FROM (SELECT id, tenant_id, po_id, vendor_id, number, currency,
				      total_cents, status, created_at, created_by
			      FROM procurement.invoices
			      `+listPredicates+`
			      ORDER BY created_at DESC, id DESC
			      LIMIT $3 OFFSET $4) c
			ORDER BY c.created_at DESC, c.id DESC`,
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
