package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luksluki/eprocurement/internal/platform/scalegate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
)

// Scale fixture ids isolate the ~100k-row fixture from every other test's
// data (the eeeeeeee family is this package family's test-tenant prefix;
// this package takes the 0052 slot).
const (
	scaleTenant = "eeeeeeee-0052-0000-0000-000000000052"
	scaleUser   = "eeeeeeee-0052-0000-0000-0000000000aa"
)

// summaryBudgetMs is the whole-summary budget the dashboard must meet at
// ~100k rows — the PRD §19 NFR table's "dashboard p95 < 500 ms" (FR-09).
const summaryBudgetMs = 500

// shapeAttempts bounds the budget's retries. The budget is a steady-state
// p95 claim, but CI runs `go test ./...` with the 100k budget fixtures
// (vendor #27, requisition #32, purchase order #38, this one) loading
// concurrently against one database, and a neighbour's bulk-load burst or
// CHECKPOINT can spike a timed loop — the #38 protocol (dev-notes 038/041,
// maintainer-approved for #52): warm-ups + 20 timed iterations, a bust
// retries after a cooldown, the final attempt decisive.
const (
	shapeAttempts   = 3
	shapeRetryPause = 5 * time.Second
)

// scaleFixture is the deterministic shape seedScale writes and the
// reconciliation expectations it computes while writing — every expected
// number is tracked, never re-derived from the summary under test.
type scaleFixture struct {
	pendingTotal   int
	pendingNumbers []string // the first page, oldest-submitted first
	dist           map[string]int64
	spend          map[spendKey]*spendAcc
}

type spendKey struct {
	vendor, currency string
}

type spendAcc struct {
	thirty, ninety int64
}

// scaleReqID builds the deterministic requisition id of fixture row i; the
// steps and PO rows reference these ids and every group stays valid hex.
func scaleReqID(i int) string {
	return fmt.Sprintf("eeeeee52-0000-0000-%04d-%012d", i%10000, i)
}

// scaleVendorID builds the deterministic vendor id of fixture slot v.
func scaleVendorID(v int) string {
	return fmt.Sprintf("eeeeee52-%04d-0000-0000-0000000000%02d", v, v)
}

// scaleStepless reports the submitted rows that carry NO approval steps: a
// shape the domain never leaves behind (decide moves the requisition), which
// the pending count's EXISTS guard must not count.
func scaleStepless(i int) bool { return i%5 == 4 && i%4000 == 4 }

// scalePO reports whether requisition row i carries a purchase order (the
// closed/approved two fifths).
func scalePO(i int) bool { return i%5 == 0 || i%5 == 1 }

// scaleDraftPO / scaleCancelledPO override the lifecycle cycle: i%20==0 is
// a draft never issued, i%20==15 an issued-then-cancelled PO — both must
// drop out of the spend windows (and the draft out of the register basis
// entirely). Both conditions fire on PO rows only (0%5==0, 15%5==0).
func scaleDraftPO(i int) bool     { return i%20 == 0 }
func scaleCancelledPO(i int) bool { return i%20 == 15 }

// TestStoreSummaryScale100k proves the #52 acceptance criteria on one
// ~100k-row fixture in the procurement.* schema:
//
//  1. Reconciliation — the summary's pending total equals the current
//     pending steps, the distribution sums to the unfiltered
//     ListPurchaseOrders total, and the spend rows/totals equal the tracked
//     per-vendor+currency sums over the register basis.
//  2. Performance — one Summary call (one transaction, five queries) stays
//     under the 500 ms p95 budget across 20 timed iterations after warm-ups.
//  3. Plans — the EXPLAIN (ANALYZE, BUFFERS) of every query is logged for
//     the dev note (the dev-note #41 evidence protocol).
func TestStoreSummaryScale100k(t *testing.T) {
	db := openTestDB(t)
	s := NewStore(db)

	// Serialize the ~100k fixtures' LOAD phase (see
	// internal/platform/scalegate): four concurrent bulk loads starved every
	// one past go test's package timeout (CI run 41198373932). The gate is
	// released after the steady-state prep so the short, retry-protected
	// measurement loops overlap instead of queueing (whole-body
	// serialization re-busts the timeout from the other side — the queued
	// package pays the wait plus its own run against 10m). The t.Cleanup
	// release is the early-exit safety net; the explicit release below is
	// the real one.
	releaseGate, err := scalegate.Acquire(t.Context(), db)
	if err != nil {
		t.Fatalf("scale gate: %v", err)
	}
	t.Cleanup(releaseGate)

	cleanupScale(t, s)
	fixture, err := seedScale(t, s, 100_000)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { cleanupScale(t, s) })

	// Production steady state before measuring (the #32/#38 fixture
	// hygiene): VACUUM (ANALYZE) sets the visibility map and statistics
	// after a bulk load, CHECKPOINT flushes the fixture's dirty buffers so
	// the background checkpointer does not compete with the timing loop.
	for _, table := range []string{
		"procurement.requisitions", "procurement.approval_steps", "procurement.purchase_orders",
	} {
		if _, err := s.db.Exec(t.Context(), `VACUUM (ANALYZE) `+table); err != nil {
			t.Fatalf("vacuum analyze %s: %v", table, err)
		}
	}
	if _, err := s.db.Exec(t.Context(), `CHECKPOINT`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	releaseGate() // load phase done — measurements overlap under the retry protocol

	now := time.Now().UTC()

	t.Run("reconciles with the fixture and the PO list", func(t *testing.T) {
		summary, err := s.Summary(t.Context(), scaleTenant, now)
		if err != nil {
			t.Fatalf("summary: %v", err)
		}

		// Pending: the count and the first page match the tracked queue.
		if summary.Pending.Total != fixture.pendingTotal {
			t.Fatalf("pending total = %d, want %d", summary.Pending.Total, fixture.pendingTotal)
		}
		if len(summary.Pending.Items) != PendingPageSize {
			t.Fatalf("pending first page = %d rows, want %d", len(summary.Pending.Items), PendingPageSize)
		}
		for i, item := range summary.Pending.Items {
			if item.Number != fixture.pendingNumbers[i] {
				t.Fatalf("pending[%d] = %s, want oldest-first %s", i, item.Number, fixture.pendingNumbers[i])
			}
			if item.StepNo != 1 || item.RoleRequired != "approver" || item.StepID == "" {
				t.Fatalf("pending[%d] step = no:%d role:%s id:%q, want step 1 / approver / non-empty",
					i, item.StepNo, item.RoleRequired, item.StepID)
			}
		}

		// Distribution: tracked counts per status, and the sum equals the
		// unfiltered ListPurchaseOrders total (the list reconciliation the
		// AC names).
		distSum := int64(0)
		for _, d := range summary.POStatusDistribution {
			if d.Count != fixture.dist[d.Status] {
				t.Fatalf("distribution[%s] = %d, want %d", d.Status, d.Count, fixture.dist[d.Status])
			}
			distSum += d.Count
		}
		filter, err := purchaseorder.ValidateList(purchaseorder.ListFilter{PageSize: 1})
		if err != nil {
			t.Fatalf("validate list: %v", err)
		}
		_, listTotal, err := purchaseorder.NewStore(s.db).List(t.Context(), scaleTenant, filter)
		if err != nil {
			t.Fatalf("po list: %v", err)
		}
		if distSum != int64(listTotal) {
			t.Fatalf("distribution sum = %d, want the unfiltered PO list total %d", distSum, listTotal)
		}

		// Spend: the bounded table matches the tracked per-vendor+currency
		// sums (largest 90-day first) and the per-currency totals match the
		// unbounded sums — which must EXCEED the bounded rows' sums here,
		// because the fixture's vendor+currency pairs outnumber
		// SpendVendorLimit (the KPI/table split is observable, not
		// decorative).
		if len(summary.SpendByVendor) != SpendVendorLimit {
			t.Fatalf("spend rows = %d, want the bound %d", len(summary.SpendByVendor), SpendVendorLimit)
		}
		prev := int64(-1)
		bounded90 := map[string]int64{}
		for i, row := range summary.SpendByVendor {
			key := spendKey{row.VendorID, row.Currency}
			want := fixture.spend[key]
			if want == nil {
				t.Fatalf("spend[%s %s] not in the tracked fixture", row.VendorName, row.Currency)
			}
			if row.SpendCents30d != want.thirty || row.SpendCents90d != want.ninety {
				t.Fatalf("spend[%s %s] = (%d, %d), want (%d, %d)",
					row.VendorName, row.Currency, row.SpendCents30d, row.SpendCents90d, want.thirty, want.ninety)
			}
			if i > 0 && row.SpendCents90d > prev {
				t.Fatalf("spend rows not by 90-day spend descending at %d: %d after %d", i, row.SpendCents90d, prev)
			}
			prev = row.SpendCents90d
			bounded90[row.Currency] += row.SpendCents90d
		}
		totals := map[string]*spendAcc{}
		for key, acc := range fixture.spend {
			if _, ok := totals[key.currency]; !ok {
				totals[key.currency] = &spendAcc{}
			}
			totals[key.currency].thirty += acc.thirty
			totals[key.currency].ninety += acc.ninety
		}
		if len(summary.SpendTotals) != len(totals) {
			t.Fatalf("spend totals = %+v, want one per currency (%d)", summary.SpendTotals, len(totals))
		}
		for _, c := range summary.SpendTotals {
			want := totals[c.Currency]
			if c.ThirtyDayCents != want.thirty || c.NinetyDayCents != want.ninety {
				t.Fatalf("totals[%s] = (%d, %d), want (%d, %d)",
					c.Currency, c.ThirtyDayCents, c.NinetyDayCents, want.thirty, want.ninety)
			}
			if c.NinetyDayCents <= bounded90[c.Currency] {
				t.Fatalf("totals[%s] = %d does not exceed the bounded rows' %d — the fixture's vendor+currency pairs must overflow the %d-row bound",
					c.Currency, c.NinetyDayCents, bounded90[c.Currency], SpendVendorLimit)
			}
		}
	})

	t.Run("p95 under 500ms", func(t *testing.T) {
		var p95 time.Duration
		for attempt := 1; attempt <= shapeAttempts; attempt++ {
			// Warm-ups (fill caches; the budget is steady-state).
			for range 3 {
				if _, err := s.Summary(t.Context(), scaleTenant, now); err != nil {
					t.Fatalf("summary warm-up: %v", err)
				}
			}
			const runs = 20
			durations := make([]time.Duration, 0, runs)
			for i := 0; i < runs; i++ {
				start := time.Now()
				summary, err := s.Summary(t.Context(), scaleTenant, now)
				durations = append(durations, time.Since(start))
				if err != nil {
					t.Fatalf("summary: %v", err)
				}
				if summary.Pending.Total == 0 || len(summary.SpendByVendor) == 0 {
					t.Fatalf("summary iteration %d returned nothing", i)
				}
			}
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			median := durations[len(durations)/2]
			p95 = durations[(len(durations)*95)/100]
			t.Logf("summary: attempt %d/%d: median=%v p95=%v over %d runs (budget %dms)",
				attempt, shapeAttempts, median, p95, len(durations), summaryBudgetMs)
			if p95.Milliseconds() < summaryBudgetMs {
				explainSummary(t, s, now)
				return
			}
			if attempt < shapeAttempts {
				t.Logf("p95 over budget — likely fixture contention, retrying after %v", shapeRetryPause)
				time.Sleep(shapeRetryPause)
			}
		}
		t.Fatalf("summary p95=%v exceeds the %dms budget", p95, summaryBudgetMs)
	})
}

// seedScale bulk-loads the ~100k fixture under the RLS bypass context via
// batched multi-row INSERTs (the #27/#32/#38 harness). Shape, all
// deterministic in the row index i (1..n):
//
//   - requisitions: i%5==0 closed · 1 approved · 2 rejected · 3 draft · 4
//     submitted. Every row's submitted_at is now-(n-i)*30d/n — monotonic in
//     i, so the queue's oldest-first order is generation order. Submitted
//     rows carry two pending steps (the current one is step 1 / approver),
//     except the scaleStepless rows, which carry none.
//   - steps: two pending steps per SUBMITTED, non-stepless requisition only
//     (the dashboard's queries read pending steps and their submitted
//     parents and nothing else — steps on approved/rejected/closed rows
//     would add 120k bulk-load rows no query under test ever touches).
//   - POs: one per closed/approved row (scalePO). Vendor i%16 over the 16
//     seeded vendors (24 vendor+currency pairs land inside the 90-day
//     window — more than SpendVendorLimit), USD when i%7==0 else IDR, total
//     1_000_000+i. Status: the five-value lifecycle cycle by (i/5)%5, with
//     scaleDraftPO a never-issued draft and scaleCancelledPO an
//     issued-then-cancelled row. issued_at (non-draft): i%4==0 → now-10d ·
//     1 → now-45d · 2 → now-100d · 3 → now-20d — both windows and the
//     outside hold real weight.
func seedScale(t *testing.T, s *Store, n int) (*scaleFixture, error) {
	t.Helper()
	fixture := &scaleFixture{
		dist:  map[string]int64{},
		spend: map[spendKey]*spendAcc{},
	}
	now := time.Now().UTC()
	spacing := (30 * 24 * time.Hour) / time.Duration(n)

	// 16 vendors: 24 vendor+currency pairs land inside the 90-day window
	// (12 IDR + 12 USD — verified by the fixture arithmetic), more than
	// SpendVendorLimit, so the bounded table provably differs from the
	// unbounded totals.
	if err := tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
		for v := 0; v < 16; v++ {
			if _, err := tx.Exec(t.Context(), `
				INSERT INTO procurement.vendors (id, tenant_id, code, name, status, created_by)
				VALUES ($1, $2, $3, $4, 'active', $5) ON CONFLICT DO NOTHING`,
				scaleVendorID(v), scaleTenant, fmt.Sprintf("SCALE-52-%02d", v),
				fmt.Sprintf("Scale Vendor 52-%02d", v), scaleUser); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("vendors: %w", err)
	}

	reqStatus := func(i int) string {
		switch i % 5 {
		case 0:
			return "closed"
		case 1:
			return "approved"
		case 2:
			return "rejected"
		case 3:
			return "draft"
		default:
			return "submitted"
		}
	}

	const batchSize = 500
	type reqRow struct {
		id, status string
		stepless   bool // the submitted rows that carry no steps (scaleStepless)
	}
	var reqs []reqRow

	for start := 1; start <= n; start += batchSize {
		end := min(start+batchSize-1, n)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*4)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.requisitions (id, tenant_id, number, requester_id, status, currency, total_cents, submitted_at) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			status := reqStatus(i)
			id := scaleReqID(i)
			stepless := status == "submitted" && scaleStepless(i)
			var submittedAt any
			if status != "draft" {
				submittedAt = now.Add(-time.Duration(n-i) * spacing)
			}
			if status == "submitted" && !stepless {
				fixture.pendingTotal++
				if len(fixture.pendingNumbers) < PendingPageSize {
					fixture.pendingNumbers = append(fixture.pendingNumbers, fmt.Sprintf("PR-52%05d", i))
				}
			}
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			// $n = id, $n+1 = number, $n+2 = requester, $n+3 = submitted_at;
			// tenant/status/currency/total are deterministic inline values.
			fmt.Fprintf(&sb, "($%d,'%s',$%d,$%d,'%s','IDR',%d,$%d::timestamptz)",
				rowNum*4-3, scaleTenant, rowNum*4-2, rowNum*4-1, status, 1_000_000+int64(i), rowNum*4)
			args = append(args, id, fmt.Sprintf("PR-52%05d", i), scaleUser, submittedAt)
			reqs = append(reqs, reqRow{id: id, status: status, stepless: stepless})
		}
		if err := commitScaleBatch(t, s, sb.String(), args); err != nil {
			return nil, fmt.Errorf("requisitions %d-%d: %w", start, end, err)
		}
	}

	// Steps: two pending per submitted, non-stepless requisition only (see
	// the shape comment — other statuses' chains load the bulk insert
	// without touching any query under test).
	for start := 0; start < len(reqs); start += batchSize {
		end := min(start+batchSize-1, len(reqs)-1)
		var (
			sb   strings.Builder
			args = make([]any, 0, batchSize*2)
		)
		sb.WriteString(`INSERT INTO procurement.approval_steps (requisition_id, step_no, role_required, threshold_cents, status) VALUES `)
		rowNum := 0
		for _, r := range reqs[start : end+1] {
			if r.status != "submitted" || r.stepless {
				continue
			}
			rowNum++
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "($%d,1,'approver',0,'pending')", rowNum*2-1)
			args = append(args, r.id)
			fmt.Fprintf(&sb, ",($%d,2,'admin',0,'pending')", rowNum*2)
			args = append(args, r.id)
		}
		if rowNum > 0 {
			if err := commitScaleBatch(t, s, sb.String(), args); err != nil {
				return nil, fmt.Errorf("approval steps %d-%d: %w", start, end, err)
			}
		}
	}

	// POs: one per closed/approved row, windows and statuses per i.
	for start := 1; start <= n; start += batchSize {
		end := min(start+batchSize-1, n)
		var (
			sb   strings.Builder
			args = make([]any, 0, batchSize*2)
		)
		sb.WriteString(`INSERT INTO procurement.purchase_orders (tenant_id, number, requisition_id, vendor_id, status, currency, total_cents, issued_at, created_by) VALUES `)
		rowNum := 0
		for i := start; i <= end; i++ {
			if !scalePO(i) {
				continue
			}
			status := []string{"issued", "acknowledged", "partially_received", "received", "closed"}[(i/5)%5]
			var issuedAt any
			switch {
			case scaleDraftPO(i):
				status = "draft" // never issued: outside the register basis
			case scaleCancelledPO(i):
				status = "cancelled" // issued, then cancelled: excluded from spend
			}
			if status != "draft" {
				switch i % 4 {
				case 0:
					issuedAt = now.Add(-10 * 24 * time.Hour)
				case 1:
					issuedAt = now.Add(-45 * 24 * time.Hour)
				case 2:
					issuedAt = now.Add(-100 * 24 * time.Hour)
				case 3:
					issuedAt = now.Add(-20 * 24 * time.Hour)
				}
			}
			currency := "IDR"
			if i%7 == 0 {
				currency = "USD"
			}
			total := int64(1_000_000 + i)
			rowNum++
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			// $n = number, $n+1 = issued_at; the rest are deterministic
			// inline values (tenant, requisition, vendor, status, currency,
			// total, creator).
			fmt.Fprintf(&sb, "('%s',$%d,'%s','%s','%s','%s',%d,$%d,'%s')",
				scaleTenant, rowNum*2-1, scaleReqID(i), scaleVendorID(i%16),
				status, currency, total, rowNum*2, scaleUser)
			args = append(args, fmt.Sprintf("PO-52%05d", i), issuedAt)

			fixture.dist[status]++
			// The tracker mirrors the store's WHERE: only issued,
			// not-cancelled POs inside the 90-day window enter the spend
			// map at all (the 30-day arm is its subset).
			if issued, ok := issuedAt.(time.Time); ok && status != "cancelled" &&
				!issued.Before(now.Add(-SpendWindow90d)) {
				key := spendKey{scaleVendorID(i % 16), currency}
				acc := fixture.spend[key]
				if acc == nil {
					acc = &spendAcc{}
					fixture.spend[key] = acc
				}
				acc.ninety += total
				if !issued.Before(now.Add(-SpendWindow30d)) {
					acc.thirty += total
				}
			}
		}
		if rowNum > 0 {
			if err := commitScaleBatch(t, s, sb.String(), args); err != nil {
				return nil, fmt.Errorf("purchase orders %d-%d: %w", start, end, err)
			}
		}
	}
	return fixture, nil
}

// commitScaleBatch runs one batched INSERT in its own bypass transaction.
func commitScaleBatch(t *testing.T, s *Store, query string, args []any) error {
	t.Helper()
	return tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), query, args...)
		return err
	})
}

// deleteScaleFixture removes the scale fixture's rows in FK-safe order
// (POs before the requisitions they reference, both counters, the vendors).
func deleteScaleFixture(tx pgx.Tx) error {
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM procurement.purchase_orders WHERE tenant_id = $1`,
		`DELETE FROM procurement.po_counters WHERE tenant_id = $1`,
		`DELETE FROM procurement.requisitions WHERE tenant_id = $1`,
		`DELETE FROM procurement.requisition_counters WHERE tenant_id = $1`,
		`DELETE FROM procurement.vendors WHERE tenant_id = $1 AND code LIKE 'SCALE-52-%'`,
	} {
		if _, err := tx.Exec(ctx, stmt, scaleTenant); err != nil {
			return err
		}
	}
	return nil
}

// cleanupScale removes the scale fixture in FK-safe order and reclaims the
// dead tuples so a re-run measures live rows only.
func cleanupScale(t *testing.T, s *Store) {
	t.Helper()
	// One retry: right after a 300k-row bulk load the compose PostgreSQL
	// (16.15) intermittently answers the big DELETE with "MultiXactId … has
	// not been created yet -- apparent wraparound" (XX000) — transient, a
	// fresh transaction seconds later succeeds. A silent failure here would
	// leave the fixture behind and break the next run on duplicate keys, so
	// the retry is load-bearing, not cosmetic.
	err := tenant.WithBypass(context.Background(), s.db, deleteScaleFixture)
	if err != nil {
		t.Logf("cleanupScale delete: %v — retrying once after a pause", err)
		time.Sleep(2 * time.Second)
		err = tenant.WithBypass(context.Background(), s.db, deleteScaleFixture)
	}
	if err != nil {
		t.Fatalf("cleanupScale delete failed twice: %v", err)
	}
	for _, table := range []string{
		"procurement.purchase_orders", "procurement.requisitions", "procurement.approval_steps",
	} {
		_, _ = s.db.Exec(context.Background(), `VACUUM ANALYZE `+table)
	}
}

// explainSummary logs the EXPLAIN (ANALYZE, BUFFERS) of every summary query
// under the real RLS tenant context — the evidence mirrored in the schema
// docs and the dev note (the dev-note #41 protocol).
func explainSummary(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	shapes := []struct {
		name  string
		query string
		args  []any
	}{
		{"pending count", pendingCountSQL, nil},
		{"pending first page", pendingPageSQL, []any{PendingPageSize}},
		{"spend totals per currency", spendTotalsSQL, []any{now.Add(-SpendWindow30d), now.Add(-SpendWindow90d)}},
		{"spend by vendor", spendByVendorSQL, []any{now.Add(-SpendWindow30d), now.Add(-SpendWindow90d), SpendVendorLimit}},
		{"po status distribution", distributionSQL, nil},
	}
	for _, shape := range shapes {
		var plan strings.Builder
		err := tenant.WithTenant(t.Context(), s.db, scaleTenant, func(tx pgx.Tx) error {
			rows, err := tx.Query(t.Context(), "EXPLAIN (ANALYZE, BUFFERS) "+shape.query, shape.args...)
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
			t.Fatalf("explain %s: %v", shape.name, err)
		}
		t.Logf("EXPLAIN (ANALYZE, BUFFERS) %s:\n%s", shape.name, plan.String())
	}
}
