// Package scalegate serializes the heavy ~100k scale fixtures across the
// test binaries CI runs in parallel. Each fixture bulk-loads hundreds of
// thousands of rows against the one shared CI PostgreSQL; since #52 a fourth
// fixture (dashboard) joined the three existing ones (vendor #27,
// requisition #32, purchase order #38), and four concurrent bulk loads
// stretched every one of them past go test's 10-minute package timeout (CI
// run 41198373932: all four procurement scale packages FAILed at 600 s).
//
// The gate covers the LOAD phase only (leading cleanup → seed → VACUUM /
// ANALYZE / CHECKPOINT): the bulk loads are the serial bottleneck, while the
// measurement loops that follow are short, retry-protected
// (shapeAttempts=3), and historically proven under 3-way overlap — the
// green reference run 41143545882 measured 3 concurrent fixtures in a
// 6m16s Test step. Serializing whole test bodies instead would queue the
// fourth package behind ~10+ minutes of neighbours and bust the same
// package timeout from the other side (observed locally, run 3).
//
// Test-support only: no production code imports this.
package scalegate

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Key is the advisory-lock id every ~100k scale fixture shares.
const Key int64 = 0x3130304B // "100K"

// Acquire blocks until the scale gate is free and then holds it on a
// dedicated pooled connection (session-scoped, so it survives across the
// test's transactions and dies with the connection if the test binary is
// killed). The returned release func is IDEMPOTENT (safe to call explicitly
// after the load phase AND from a t.Cleanup safety net — cleanups run LIFO,
// so a fixture's delete registered after Acquire would still run under the
// gate if the test died before the explicit release). The release uses
// context.Background because the testing context is canceled just before
// cleanups run.
func Acquire(ctx context.Context, db *pgxpool.Pool) (func(), error) {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("scalegate: acquire conn: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", Key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("scalegate: lock: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", Key)
			conn.Release()
		})
	}, nil
}
