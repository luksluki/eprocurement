package vendor

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// seedListVendors inserts vendors for the filter tests and removes them
// afterwards. Codes carry a shared prefix so cleanup is exact.
const listPrefix = "L27-"

func seedListVendors(t *testing.T, s *Store) {
	t.Helper()
	rows := []struct{ code, name string }{
		{"L27-ALPHA", "PT Alpha Nusantara"},
		{"L27-ALPACA", "PT Alpaka Sejahtera"},
		{"L27-BRAVO", "CV Bravo Mandiri"},
		{"L27-CHARLIE", "PT Charlie Nusantara"},
		{"L27-DELTA", "PT Delta Persada"},
		{"L27-PERCENT", "PT 100% Ampuh"},
	}
	for _, r := range rows {
		_, err := s.Create(t.Context(), tenantA, CreateInput{
			Code: r.code, Name: r.name, UserID: userA,
		})
		if err != nil {
			t.Fatalf("create %s: %v", r.code, err)
		}
	}
	// Deactivate two so the status filter has both values.
	for _, code := range []string{"L27-BRAVO", "L27-CHARLIE"} {
		if _, err := s.Deactivate(t.Context(), tenantA, code2id(t, s, code), userA); err != nil {
			t.Fatalf("deactivate %s: %v", code, err)
		}
	}
	// Another tenant's vendor: never visible to tenantA listings.
	if _, err := s.Create(t.Context(), tenantB, CreateInput{
		Code: "L27-ALPHA", Name: "PT Alpha Nusantara", UserID: userA,
	}); err != nil {
		t.Fatalf("create tenantB: %v", err)
	}

	t.Cleanup(func() {
		_ = tenant.WithBypass(context.Background(), s.db, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`DELETE FROM procurement.vendors WHERE code LIKE $1`, listPrefix+"%")
			return err
		})
	})
}

func code2id(t *testing.T, s *Store, code string) string {
	t.Helper()
	v, err := s.Get(t.Context(), tenantA, byCode(t, s, code))
	if err != nil {
		t.Fatalf("get %s: %v", code, err)
	}
	return v.ID
}

// byCode resolves a vendor id via the unique (tenant, code) index.
func byCode(t *testing.T, s *Store, code string) string {
	t.Helper()
	var id string
	err := tenant.WithTenant(t.Context(), s.db, tenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT id FROM procurement.vendors WHERE code = $1`, code).Scan(&id)
	})
	if err != nil {
		t.Fatalf("byCode %s: %v", code, err)
	}
	return id
}

func listOK(t *testing.T, s *Store, f ListFilter) ([]Vendor, int) {
	t.Helper()
	filter, err := ValidateList(f)
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	vendors, total, err := s.List(t.Context(), tenantA, filter)
	if err != nil {
		t.Fatalf("List(%+v): %v", f, err)
	}
	return vendors, total
}

func TestStoreListFilters(t *testing.T) {
	s := testStore(t)
	seedListVendors(t, s)

	// No filter: all of tenantA's L27 vendors, ordered by code.
	vendors, total := listOK(t, s, ListFilter{PageSize: 10})
	if total != 6 || len(vendors) != 6 {
		t.Fatalf("unfiltered = %d rows (total %d), want 6", len(vendors), total)
	}
	for i := 1; i < len(vendors); i++ {
		if vendors[i-1].Code >= vendors[i].Code {
			t.Fatalf("not ordered by code: %s then %s", vendors[i-1].Code, vendors[i].Code)
		}
	}

	// Status filter.
	vendors, total = listOK(t, s, ListFilter{Status: StatusActive, PageSize: 10})
	if total != 4 || vendors[0].Code != "L27-ALPACA" || vendors[3].Code != "L27-PERCENT" {
		t.Fatalf("active = %d rows (total %d): %+v", len(vendors), total, codes(vendors))
	}
	vendors, total = listOK(t, s, ListFilter{Status: StatusDeactivated, PageSize: 10})
	if total != 2 || vendors[0].Code != "L27-BRAVO" || vendors[1].Code != "L27-CHARLIE" {
		t.Fatalf("deactivated = %+v (total %d)", codes(vendors), total)
	}

	// Query: code prefix, case-insensitive input.
	vendors, total = listOK(t, s, ListFilter{Query: "l27-alp", PageSize: 10})
	if total != 2 || vendors[0].Code != "L27-ALPACA" || vendors[1].Code != "L27-ALPHA" {
		t.Fatalf("code prefix = %+v (total %d)", codes(vendors), total)
	}

	// Query: name substring, case-insensitive.
	vendors, total = listOK(t, s, ListFilter{Query: "nusantara", PageSize: 10})
	if total != 2 || vendors[0].Code != "L27-ALPHA" || vendors[1].Code != "L27-CHARLIE" {
		t.Fatalf("name substring = %+v (total %d)", codes(vendors), total)
	}

	// Query: % is literal, not a wildcard (only "PT 100% Ampuh" contains it).
	vendors, total = listOK(t, s, ListFilter{Query: "100%", PageSize: 10})
	if total != 1 || vendors[0].Code != "L27-PERCENT" {
		t.Fatalf("literal %% = %+v (total %d)", codes(vendors), total)
	}

	// Combined: status + query (name substring among active only).
	vendors, total = listOK(t, s, ListFilter{Query: "nusantara", Status: StatusActive, PageSize: 10})
	if total != 1 || vendors[0].Code != "L27-ALPHA" {
		t.Fatalf("combined = %+v (total %d)", codes(vendors), total)
	}
	// Combined the other way: the Nusantara match inside deactivated rows.
	vendors, total = listOK(t, s, ListFilter{Query: "nusantara", Status: StatusDeactivated, PageSize: 10})
	if total != 1 || vendors[0].Code != "L27-CHARLIE" {
		t.Fatalf("combined deactivated = %+v (total %d)", codes(vendors), total)
	}
}

func TestStoreListPagination(t *testing.T) {
	s := testStore(t)
	seedListVendors(t, s)

	// Codes in order: ALPACA, ALPHA, BRAVO, CHARLIE, DELTA, PERCENT.
	// Page 2 with size 2 lands on BRAVO and CHARLIE.
	vendors, total := listOK(t, s, ListFilter{Page: 2, PageSize: 2})
	if total != 6 || len(vendors) != 2 || vendors[0].Code != "L27-BRAVO" || vendors[1].Code != "L27-CHARLIE" {
		t.Fatalf("page 2 = %+v (total %d)", codes(vendors), total)
	}

	// Page beyond the end: empty page, total still reported.
	vendors, total = listOK(t, s, ListFilter{Page: 5, PageSize: 2})
	if total != 6 || len(vendors) != 0 {
		t.Fatalf("overflow page = %d rows (total %d)", len(vendors), total)
	}

	// Defaults land on page 1 with page size 20.
	vendors, _ = listOK(t, s, ListFilter{})
	if len(vendors) != 6 || vendors[0].Code != "L27-ALPACA" {
		t.Fatalf("defaults = %+v", codes(vendors))
	}
}

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
}

func codes(vendors []Vendor) []string {
	out := make([]string, 0, len(vendors))
	for _, v := range vendors {
		out = append(out, v.Code)
	}
	return out
}
