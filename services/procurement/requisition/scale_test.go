package requisition

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

// scaleTenant/scaleUser isolate the 100k-row fixture from every other test's
// data (the eeeeeeee family is this package's test-tenant prefix).
const (
	scaleTenant = "eeeeeeee-0032-0000-0000-000000000032"
	scaleUser   = "eeeeeeee-0032-0000-0000-0000000000aa"
)

// listBudgetMs is the per-query budget the list must meet at 100k rows —
// aligned with the PRD's p95 < 300 ms performance bar (FR-09; the vendor list
// #27 is held to the same number).
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

// TestStoreListScale100k proves the #32 acceptance criterion: with 100k
// requisitions in one tenant every list shape — unfiltered, status, number
// prefix, combined — stays under budget. The EXPLAIN plans are logged below
// and mirrored in docs/schema/procurement.md.
func TestStoreListScale100k(t *testing.T) {
	s, db := testStoreDB(t)
	guard := newScaleGuard(t, db)

	if err := seedScale(t, s, guard, 100_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { cleanupScale(t, s) })

	// Bring the table to its production steady state before measuring:
	// VACUUM ANALYZE (what autovacuum does after a bulk load) sets the
	// visibility map and statistics — without it the count arm's index-only
	// scan degrades to 100k heap fetches and the planner runs on empty
	// stats. The explicit CHECKPOINT then flushes the fixture's dirty
	// buffers so the background checkpointer does not compete with the
	// timing loop (seen as hundred-ms outliers during #32). Both need
	// superuser, so they run on the pool directly — withSettings drops even
	// the bypass context to the non-superuser app role — and under the
	// shared fixture lock (see scaleLockKey): a global CHECKPOINT flushes
	// every database's buffers at once.
	guard.lock(t)
	_, err := db.Exec(t.Context(), `VACUUM (ANALYZE) procurement.requisitions`)
	guard.unlock()
	if err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
	guard.lock(t)
	_, err = db.Exec(t.Context(), `CHECKPOINT`)
	guard.unlock()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	shapes := []struct {
		name string
		f    ListFilter
	}{
		{"unfiltered page 1", ListFilter{}},
		{"status filter", ListFilter{Status: StatusSubmitted}}, // ~10% of rows
		{"number prefix", ListFilter{Query: "PR-3209"}},        // ~1k of the 100k numbers
		{"combined status+query", ListFilter{Query: "PR-32", Status: StatusDraft}},
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

// seedScale bulk-loads n requisitions for scaleTenant via batched multi-row
// INSERTs (bypassing RLS — the fixture is not the behavior under test).
// COPY FROM is not an option: PostgreSQL rejects it on RLS-enabled tables
// regardless of the bypass setting (same finding as #27). Numbers use the
// PR-32xxxxx space so a short prefix selects a known ~1k slice; statuses
// split 80% draft / 10% submitted / 10% cancelled. Batches commit one
// transaction each under the shared fixture lock (see scaleLockKey) so no
// batch's IO spike lands inside another package's budget measurement.
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
		sb.WriteString(`INSERT INTO procurement.requisitions (tenant_id, number, requester_id, status, currency, total_cents) VALUES `)
		for i := start; i <= end; i++ {
			rowNum++
			number := fmt.Sprintf("PR-32%05d", i)
			status := StatusDraft
			switch {
			case i%10 == 3:
				status = StatusCancelled
			case i%10 == 7:
				status = StatusSubmitted
			}
			if rowNum > 1 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,'IDR',1000000)", rowNum*4-3, rowNum*4-2, rowNum*4-1, rowNum*4)
			args = append(args, scaleTenant, number, scaleUser, status)
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
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisitions WHERE tenant_id = $1`, scaleTenant); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisition_counters WHERE tenant_id = $1`, scaleTenant)
		return err
	})
	// Reclaim the fixture's dead tuples so a re-run of the suite measures a
	// table of 100k live rows, not 100k live + the previous run's dead ones.
	_, _ = s.db.Exec(context.Background(), `VACUUM ANALYZE procurement.requisitions`)
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
			SELECT c.id, c.tenant_id, c.number, c.requester_id, c.status, c.currency, c.total_cents,
			       c.submitted_at, c.created_at, c.created_by, c.updated_at, c.updated_by,
			       c.submit_idempotency_key,
			       (SELECT count(*) FROM procurement.requisitions `+listPredicates+`) AS total_count
			FROM (SELECT id, tenant_id, number, requester_id, status, currency, total_cents,
			             submitted_at, created_at, created_by, updated_at, updated_by,
			             submit_idempotency_key
			      FROM procurement.requisitions
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
	t.Logf("EXPLAIN (ANALYZE) status=%q query=%q:\n%s", f.Status, f.Query, plan.String())
}
