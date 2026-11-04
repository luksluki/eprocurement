// Package pgbatch sends one fixed statement once per parameter row through a
// single pgx.Batch, so a per-item write loop costs one network round trip
// instead of one per row (issue #56's write-path N+1) while every row keeps
// its own result: the per-row error semantics of the plain loop are
// preserved exactly — the first failing row's error surfaces, the caller's
// transaction fails as a whole either way, and an early return still closes
// the batch so the transaction can roll back cleanly.
package pgbatch

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Exec queues query once per parameter row into one batch, sends it on tx in
// a single pipeline round trip, and drains the results in order. It returns
// the first per-row error (an empty rows slice is a no-op — no statement is
// sent at all, so a zero-item caller behaves like the loop it replaced).
//
// The batch is always closed before Exec returns (pgx requires a batch to be
// fully drained or closed before its transaction commits/rolls back), so
// this is safe to call anywhere inside a tenant.WithTenant transaction.
// Item counts here are domain-validated baskets (tens of rows), well under
// any pipeline-size concern.
func Exec(ctx context.Context, tx pgx.Tx, query string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(query, row...)
	}
	br := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := br.Exec(); err != nil {
			_ = br.Close() // discard the queued results behind the failure
			return err
		}
	}
	return br.Close()
}
