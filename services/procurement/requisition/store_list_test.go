package requisition

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// seedListRequisitions creates six requisitions in tenantA (four drafts, one
// submitted, one cancelled) and one in tenantB (never visible to tenantA),
// removing them afterwards via the shared cleanup helper. Numbers come from
// the per-tenant counter, so the exact sequence depends on a clean tenant —
// every list test calls cleanupRequisition first.
func seedListRequisitions(t *testing.T, s *Store) []Requisition {
	t.Helper()
	ctx := context.Background()
	var created []Requisition
	for i := 0; i < 6; i++ {
		r, err := s.Create(ctx, tenantA, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		created = append(created, r)
	}
	// The oldest becomes submitted (needs items; no rules materializer in the
	// store test).
	submitted, err := s.Submit(ctx, tenantA, created[0].ID, userA, "key-list-32", nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	created[0] = submitted
	// The newest becomes cancelled via a direct status flip — the store
	// exposes no cancel method yet; the list only reads the column.
	if err := tenant.WithBypass(ctx, s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE procurement.requisitions SET status = 'cancelled' WHERE id = $1`, created[5].ID)
		return err
	}); err != nil {
		t.Fatalf("cancel fixture: %v", err)
	}
	created[5].Status = StatusCancelled
	// Another tenant's row: never visible to tenantA listings.
	if _, err := s.Create(ctx, tenantB, CreateInput{RequesterID: userA, Currency: "IDR", Items: basketItems}); err != nil {
		t.Fatalf("create tenantB: %v", err)
	}
	return created
}

func listOK(t *testing.T, s *Store, f ListFilter) ([]Requisition, int) {
	t.Helper()
	filter, err := ValidateList(f)
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	rows, total, err := s.List(t.Context(), tenantA, filter)
	if err != nil {
		t.Fatalf("List(%+v): %v", f, err)
	}
	return rows, total
}

func numbers(rows []Requisition) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Number)
	}
	return out
}

// Filters combine correctly: status equality, number prefix (case-insensitive
// input, literal wildcards), and both orderings of the combination; the list
// stays tenant-scoped; rows are header-only.
func TestStoreListFilters(t *testing.T) {
	s, db := testStoreDB(t)
	cleanupRequisition(t, db)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	created := seedListRequisitions(t, s)

	// No filter: all six of tenantA's rows, newest number first.
	rows, total := listOK(t, s, ListFilter{PageSize: 10})
	if total != 6 || len(rows) != 6 {
		t.Fatalf("unfiltered = %d rows (total %d), want 6", len(rows), total)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Number <= rows[i].Number {
			t.Fatalf("not descending: %s then %s", rows[i-1].Number, rows[i].Number)
		}
	}
	// Header-only: the list never loads items even for requisitions that have them.
	for _, r := range rows {
		if len(r.Items) != 0 {
			t.Fatalf("list row carries items: %+v", r)
		}
	}

	// Status filter.
	rows, total = listOK(t, s, ListFilter{Status: StatusDraft, PageSize: 10})
	if total != 4 || rows[0].Status != StatusDraft {
		t.Fatalf("draft = %d rows (total %d): %+v", len(rows), total, numbers(rows))
	}
	rows, total = listOK(t, s, ListFilter{Status: StatusSubmitted, PageSize: 10})
	if total != 1 || rows[0].ID != created[0].ID || rows[0].SubmittedAt == nil {
		t.Fatalf("submitted = %+v (total %d)", numbers(rows), total)
	}

	// Query: number prefix, case-insensitive input — matches exactly one number.
	rows, total = listOK(t, s, ListFilter{Query: created[1].Number, PageSize: 10})
	if total != 1 || rows[0].ID != created[1].ID {
		t.Fatalf("full number = %+v (total %d)", numbers(rows), total)
	}

	// Query: the common prefix of the six numbers matches them all.
	prefix := commonPrefix(created)
	if len(prefix) < 4 {
		t.Fatalf("common prefix %q suspiciously short", prefix)
	}
	rows, total = listOK(t, s, ListFilter{Query: prefix, PageSize: 10})
	if total != 6 {
		t.Fatalf("prefix %s = %+v (total %d)", prefix, numbers(rows), total)
	}

	// Query: % is literal, not a wildcard (no number contains one; this also
	// exercises the past-the-end COUNT fallback with zero matches).
	rows, total = listOK(t, s, ListFilter{Query: "%", PageSize: 10})
	if total != 0 || len(rows) != 0 {
		t.Fatalf("literal %% = %+v (total %d)", numbers(rows), total)
	}

	// Combined: status + prefix (the four drafts share the prefix).
	rows, total = listOK(t, s, ListFilter{Query: prefix, Status: StatusDraft, PageSize: 10})
	if total != 4 {
		t.Fatalf("combined draft = %+v (total %d)", numbers(rows), total)
	}
	// Combined the other way: the cancelled row among the prefixed set.
	rows, total = listOK(t, s, ListFilter{Query: prefix, Status: StatusCancelled, PageSize: 10})
	if total != 1 || rows[0].ID != created[5].ID {
		t.Fatalf("combined cancelled = %+v (total %d)", numbers(rows), total)
	}

	// Cross-tenant: tenantA's rows never appear in tenantB's listing, and
	// tenantB sees exactly its own single row.
	rowsB, totalB, err := s.List(t.Context(), tenantB, mustValidate(t, ListFilter{Query: "PR-"}))
	if err != nil {
		t.Fatalf("tenantB list: %v", err)
	}
	if totalB != 1 || len(rowsB) != 1 || rowsB[0].TenantID != tenantB {
		t.Fatalf("tenantB sees %d rows (total %d), want its single row", len(rowsB), totalB)
	}
}

// Pagination: deep pages land correctly, a page past the end is empty but
// still reports the true total (the COUNT fallback), and zero values apply
// the defaults (page 1, page size 20).
func TestStoreListPagination(t *testing.T) {
	s, db := testStoreDB(t)
	cleanupRequisition(t, db)
	t.Cleanup(func() { cleanupRequisition(t, db) })
	created := seedListRequisitions(t, s)

	// Descending order: cancelled (newest), drafts c4..c1, submitted (oldest).
	// Page 2 with size 2 = the third- and fourth-newest rows.
	rows, total := listOK(t, s, ListFilter{Page: 2, PageSize: 2})
	if total != 6 || len(rows) != 2 ||
		rows[0].ID != created[3].ID || rows[1].ID != created[2].ID {
		t.Fatalf("page 2 = %+v (total %d)", numbers(rows), total)
	}

	// Page beyond the end: empty page, total still reported.
	rows, total = listOK(t, s, ListFilter{Page: 5, PageSize: 2})
	if total != 6 || len(rows) != 0 {
		t.Fatalf("overflow page = %d rows (total %d)", len(rows), total)
	}

	// Defaults land on page 1 with page size 20 (the newest row first).
	rows, _ = listOK(t, s, ListFilter{})
	if len(rows) != 6 || rows[0].ID != created[5].ID {
		t.Fatalf("defaults = %+v", numbers(rows))
	}
}

// ValidateList rejects unknown statuses and absurd pages, and clamps the rest.
func TestValidateListErrors(t *testing.T) {
	if _, err := ValidateList(ListFilter{Status: "paused"}); err != ErrStatusInvalid {
		t.Fatalf("status error = %v", err)
	}
	if _, err := ValidateList(ListFilter{Page: MaxPage + 1}); err != ErrPageInvalid {
		t.Fatalf("page error = %v", err)
	}

	// Defaults are applied for zero values; page_size is capped.
	f, err := ValidateList(ListFilter{PageSize: 500})
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	if f.Page != 1 || f.PageSize != MaxPageSize {
		t.Fatalf("normalized filter = %+v", f)
	}
	// Query is trimmed and uppercased (numbers are canonical uppercase).
	f, err = ValidateList(ListFilter{Query: "  pr-0001  "})
	if err != nil || f.Query != "PR-0001" {
		t.Fatalf("normalized query = %+v (%v)", f, err)
	}
}

func mustValidate(t *testing.T, f ListFilter) ListFilter {
	t.Helper()
	filter, err := ValidateList(f)
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	return filter
}

// commonPrefix returns the longest prefix shared by all seeded numbers.
func commonPrefix(created []Requisition) string {
	prefix := created[0].Number
	for _, r := range created[1:] {
		for len(prefix) > 0 && (len(r.Number) < len(prefix) || r.Number[:len(prefix)] != prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}
