package vendor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// scaleTenant isolates the 100k-row fixture from every other test's data.
const scaleTenant = "cccccccc-0027-0000-0000-000000000027"

// listBudgetMs is the per-query budget the list must meet at 100k rows —
// aligned with the PRD's p95 < 300 ms performance budget (FR-09 sets it for
// dashboard reads; the vendor list is held to the same bar).
const listBudgetMs = 300

// TestStoreListScale100k proves the #27 acceptance criteria: with 100k
// vendor rows in one tenant every list shape — unfiltered, status, code
// prefix, name substring, combined — stays under budget, and the EXPLAIN
// plans (logged below, mirrored in docs/schema/procurement.md) show the
// indexes carrying the work instead of sequential scans.
func TestStoreListScale100k(t *testing.T) {
	s := testStore(t)

	if err := seedScale(t, s, 100_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { cleanupScale(t, s) })

	// Fresh statistics, as autovacuum would keep them in production — the
	// bulk load just happened, so without this the planner would run on
	// empty-table stats.
	if err := tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `ANALYZE procurement.vendors`)
		return err
	}); err != nil {
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

// seedScale bulk-loads n vendors for scaleTenant via COPY (bypassing RLS —
// the fixture is not the behavior under test).
// seedScale bulk-loads n vendors for scaleTenant via batched multi-row
// INSERTs (bypassing RLS — the fixture is not the behavior under test).
// COPY FROM is not an option: PostgreSQL rejects it on RLS-enabled tables
// regardless of the bypass setting.
func seedScale(t *testing.T, s *Store, n int) error {
	t.Helper()
	const batchSize = 500
	err := tenant.WithBypass(t.Context(), s.db, func(tx pgx.Tx) error {
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
			if _, err := tx.Exec(t.Context(), sb.String(), args...); err != nil {
				return err
			}
		}
		return nil
	})
	return err
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
