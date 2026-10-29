package vendor

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

// scaleTenant isolates the 100k-row fixture from every other test's data.
const scaleTenant = "cccccccc-0027-0000-0000-000000000027"

// listBudgetMs is the per-query budget the list must meet at 100k rows —
// aligned with the PRD's p95 < 300 ms performance budget (FR-09 sets it for
// dashboard reads; the vendor list is held to the same bar).
const listBudgetMs = 300

// scaleLockKey is the advisory-lock key shared by the three 100k budget
// fixtures (vendor #27, requisition #32, purchaseorder #38). `go test ./...`
// runs their packages concurrently against one database, and their combined
// bulk-load/IO spikes pushed neighbouring timed loops over the p95 budget
// (CI run 40797112295: this test's unfiltered shape measured p95 322 ms —
// median 209 ms vs ~73 ms solo — while all three fixtures bulk-loaded at
// once). Every heavy unit (a seed batch, the ANALYZE hygiene, one
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

// TestStoreListScale100k proves the #27 acceptance criteria: with 100k
// vendor rows in one tenant every list shape — unfiltered, status, code
// prefix, name substring, combined — stays under budget, and the EXPLAIN
// plans (logged below, mirrored in docs/schema/procurement.md) show the
// indexes carrying the work instead of sequential scans.
func TestStoreListScale100k(t *testing.T) {
	s := testStore(t)
	guard := newScaleGuard(t, s.db)

	if err := seedScale(t, s, guard, 100_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { cleanupScale(t, s) })

	// Fresh statistics, as autovacuum would keep them in production — the
	// bulk load just happened, so without this the planner would run on
	// empty-table stats. Under the shared fixture lock (see scaleLockKey).
	guard.lock(t)
	err := tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `ANALYZE procurement.vendors`)
		return err
	})
	guard.unlock()
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	shapes := []struct {
		name string
		f    ListFilter
	}{
		{"unfiltered page 1", ListFilter{}},
		{"status filter", ListFilter{Status: StatusDeactivated}},
		{"code prefix", ListFilter{Query: "S27-V099"}}, // ~1k of the 100k codes
		{"name substring", ListFilter{Query: "nusantara"}},
		{"combined status+query", ListFilter{Query: "nusantara", Status: StatusActive}},
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
			// One warm-up (fills caches; the budget is steady-state).
			if _, _, err := s.List(t.Context(), scaleTenant, f); err != nil {
				t.Fatalf("list: %v", err)
			}
			durations := make([]time.Duration, 0, 20)
			for i := 0; i < 20; i++ {
				start := time.Now()
				vendors, total, err := s.List(t.Context(), scaleTenant, f)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				durations = append(durations, time.Since(start))
				if len(vendors) == 0 || total == 0 {
					t.Fatalf("shape %s returned nothing (rows=%d total=%d)", shape.name, len(vendors), total)
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

// seedScale bulk-loads n vendors for scaleTenant via batched multi-row
// INSERTs (bypassing RLS — the fixture is not the behavior under test).
// COPY FROM is not an option: PostgreSQL rejects it on RLS-enabled tables
// regardless of the bypass setting. Batches commit one transaction each
// under the shared fixture lock (see scaleLockKey) so no batch's IO spike
// lands inside another package's budget measurement.
func seedScale(t *testing.T, s *Store, guard *scaleGuard, n int) error {
	t.Helper()
	const batchSize = 500
	for start := 1; start <= n; start += batchSize {
		end := min(start+batchSize-1, n)
		var (
			sb     strings.Builder
			args   = make([]any, 0, batchSize*4)
			rowNum int
		)
		sb.WriteString(`INSERT INTO procurement.vendors (tenant_id, code, name, status) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			code := fmt.Sprintf("S27-V%06d", i)
			name := fmt.Sprintf("PT Vendor %06d", i)
			if i%10 == 0 {
				name += " Nusantara"
			}
			status := StatusActive
			if i%10 == 7 { // 10% deactivated
				status = StatusDeactivated
			}
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", rowNum*4-3, rowNum*4-2, rowNum*4-1, rowNum*4)
			args = append(args, scaleTenant, code, name, status)
		}
		guard.lock(t)
		err := tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), sb.String(), args...)
			return err
		})
		guard.unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func cleanupScale(t *testing.T, s *Store) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.vendors WHERE tenant_id = $1`, scaleTenant)
		return err
	})
}

// explainList logs the EXPLAIN ANALYZE plan of the list query under the
// real RLS tenant context — the evidence mirrored in the schema docs.
func explainList(t *testing.T, s *Store, f ListFilter) {
	t.Helper()
	offset := (int(f.Page) - 1) * int(f.PageSize)
	var plan strings.Builder
	err := tenant.WithTenant(t.Context(), s.db, scaleTenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(t.Context(), `
			EXPLAIN (ANALYZE, BUFFERS)
			SELECT id, tenant_id, code, name, contact_email, tax_id, status,
			       created_at, created_by, updated_at, updated_by, count(*) OVER()
			FROM procurement.vendors
			`+listPredicates+`
			ORDER BY code
			LIMIT $3 OFFSET $4`,
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
	t.Logf("EXPLAIN (ANALYZE) status=%q query=%q:\n%s", f.Status, f.Query, plan.String())
}
