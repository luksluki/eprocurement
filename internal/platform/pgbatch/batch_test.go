package pgbatch

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// TestExecAgainstPostgres proves the helper's two contract points against the
// compose database (the table-driven happy path + failure path the store
// conventions ask for): the happy path lands every row in one round trip,
// and the failure path surfaces the failing row's error (a unique violation
// mid-batch) while leaving the transaction a normal, rollback-able failure —
// the exact semantics the per-item loops it replaces had.
func TestExecAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	const insertRow = `INSERT INTO pgbatch_scratch (n) VALUES ($1)`

	t.Run("lands every row of the batch", func(t *testing.T) {
		err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `
				CREATE TEMP TABLE pgbatch_scratch (n int CONSTRAINT pgbatch_scratch_uidx UNIQUE)
				ON COMMIT DROP`); err != nil {
				return err
			}
			if err := Exec(context.Background(), tx, insertRow, [][]any{{1}, {2}, {3}}); err != nil {
				return err
			}
			var n int
			if err := tx.QueryRow(context.Background(),
				`SELECT count(*) FROM pgbatch_scratch`).Scan(&n); err != nil {
				return err
			}
			if n != 3 {
				t.Fatalf("rows landed = %d, want 3", n)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("happy path: %v", err)
		}
	})

	t.Run("empty rows sends nothing", func(t *testing.T) {
		err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			// The statement names a nonexistent table: if the batch were
			// sent despite zero rows, Exec would fail on it.
			return Exec(context.Background(), tx,
				`INSERT INTO pgbatch_no_such_table (n) VALUES ($1)`, nil)
		})
		if err != nil {
			t.Fatalf("empty batch must be a no-op, got: %v", err)
		}
	})

	t.Run("returns the failing row's error and aborts the transaction", func(t *testing.T) {
		sentinel := errors.New("rollback requested")
		err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `
				CREATE TEMP TABLE pgbatch_scratch (n int CONSTRAINT pgbatch_scratch_uidx UNIQUE)
				ON COMMIT DROP`); err != nil {
				return err
			}
			execErr := Exec(context.Background(), tx, insertRow, [][]any{{7}, {7}, {8}})
			if execErr == nil {
				t.Fatal("want the duplicate row's unique-violation error")
			}
			var pgErr interface{ SQLState() string }
			if !errors.As(execErr, &pgErr) || pgErr.SQLState() != "23505" {
				t.Fatalf("error = %v, want the 23505 unique violation", execErr)
			}
			return sentinel // the rollback path every store's caller takes
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("WithBypass err = %v, want the sentinel rollback", err)
		}
	})
}
