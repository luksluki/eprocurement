package approval

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// mapDecideKeyError is a pure error mapping (no database): only the
// decision_idempotency_pkey 23505 violation becomes
// ErrIdempotencyKeyConflict; every other pg error, other constraint names,
// other codes, wrapped and plain errors pass through unchanged. Synthetic
// *pgconn.PgError values stand in for the driver's (the belt-and-braces
// twin mapping is unreachable behind the parent lock, so the store path
// cannot produce it in a test).
func TestMapDecideKeyError(t *testing.T) {
	pkViolation := &pgconn.PgError{Code: "23505", ConstraintName: "decision_idempotency_pkey"}
	otherConstraint := &pgconn.PgError{Code: "23505", ConstraintName: "requisitions_pkey"}
	otherCode := &pgconn.PgError{Code: "23503", ConstraintName: "decision_idempotency_pkey"}

	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"pk violation maps to conflict":   {pkViolation, ErrIdempotencyKeyConflict},
		"wrapped pk violation maps":       {fmt.Errorf("insert decide key: %w", pkViolation), ErrIdempotencyKeyConflict},
		"other constraint passes through": {otherConstraint, otherConstraint},
		"other pg code passes through":    {otherCode, otherCode},
		"plain error passes through":      {errors.New("connection reset"), nil},
		"nil passes through":              {nil, nil},
	} {
		got := mapDecideKeyError(tc.err)
		if tc.want == nil {
			if got != tc.err {
				t.Errorf("%s: err = %v, want it unchanged", name, got)
			}
			continue
		}
		if !errors.Is(got, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, got, tc.want)
		}
	}
}
