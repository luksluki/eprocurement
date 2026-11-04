package invoice

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNumber(t *testing.T) {
	for name, tc := range map[string]struct {
		number  string
		wantErr error
	}{
		"valid":      {"INV-2026-000042", nil},
		"free-form":  {"AB/CD-2026.001 #7", nil},
		"at bound":   {strings.Repeat("n", MaxNumberLen), nil},
		"empty":      {"", ErrNumberInvalid},
		"over bound": {strings.Repeat("n", MaxNumberLen+1), ErrNumberInvalid},
	} {
		if err := ValidateNumber(tc.number); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: ValidateNumber(%q) = %v, want %v", name, tc.number, err, tc.wantErr)
		}
	}
	// Trimming is the store's job: the shape check sees the trimmed value,
	// and the store test pins the whitespace-only rejection end-to-end.
	if err := ValidateNumber(strings.TrimSpace("  INV-1  ")); err != nil {
		t.Errorf("trimmed number: %v", err)
	}
}

// The line-shape validation, table-driven: happy path plus each failure.
func TestValidateItems(t *testing.T) {
	valid := []Item{
		{POItemID: "55555555-0000-0000-0000-0000000000a1", Qty: 2, UnitPriceCents: 7500000},
		{POItemID: "55555555-0000-0000-0000-0000000000a2", Qty: 1, UnitPriceCents: 0},
	}
	for name, tc := range map[string]struct {
		items   []Item
		wantErr error
	}{
		"valid":               {valid, nil},
		"empty":               {nil, ErrItemsEmpty},
		"zero qty":            {[]Item{{POItemID: "a", Qty: 0, UnitPriceCents: 1}}, ErrQtyInvalid},
		"negative qty":        {[]Item{{POItemID: "a", Qty: -1, UnitPriceCents: 1}}, ErrQtyInvalid},
		"negative unit price": {[]Item{{POItemID: "a", Qty: 1, UnitPriceCents: -1}}, ErrUnitPriceInvalid},
		"zero unit price ok":  {[]Item{{POItemID: "a", Qty: 1, UnitPriceCents: 0}}, nil},
		"duplicate line": {
			[]Item{
				{POItemID: "a", Qty: 1, UnitPriceCents: 1},
				{POItemID: "a", Qty: 1, UnitPriceCents: 1},
			},
			ErrPOItemDuplicate,
		},
	} {
		if err := ValidateItems(tc.items); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: ValidateItems = %v, want %v", name, err, tc.wantErr)
		}
	}
}

// The total is computed server-side from the lines — the sum of
// qty × unit_price_cents.
func TestTotalCents(t *testing.T) {
	items := []Item{
		{POItemID: "a", Qty: 2, UnitPriceCents: 7500000},
		{POItemID: "b", Qty: 3, UnitPriceCents: 150000},
	}
	if got := TotalCents(items); got != 15450000 {
		t.Fatalf("TotalCents = %d, want 15450000", got)
	}
	if got := TotalCents(nil); got != 0 {
		t.Fatalf("TotalCents(nil) = %d, want 0", got)
	}
}

// The pinned over-billing detail line, verbatim, and the sentinel match
// through the typed error — the OverReceiptError contract of #37.
func TestOverBillError(t *testing.T) {
	over := &OverBillError{PONumber: "PO-000001", Invoiced: 6000000, Requested: 6000000, Total: 10000000}
	want := "inv_over_bill: po_number=PO-000001 invoiced=6000000 requested=6000000 total=10000000"
	if over.Error() != want {
		t.Fatalf("Error() = %q, want %q", over.Error(), want)
	}
	if !errors.Is(over, ErrOverBill) {
		t.Fatalf("errors.Is(typed, ErrOverBill) = false")
	}
	if !errors.Is(over, over) {
		t.Fatalf("errors.Is(typed, itself) = false")
	}
}

// The #40 re-run guard: matched is final, pending and discrepancy may be
// (re-)matched — a late GRN resolving a discrepancy is the intended loop
// (maintainer decision 2026-10-28).
func TestMatchGuard(t *testing.T) {
	cases := []struct {
		status  string
		wantErr error
	}{
		{StatusPending, nil},
		{StatusDiscrepancy, nil},
		{StatusMatched, ErrAlreadyMatched},
		{"", nil}, // unknown statuses are not this guard's business
	}
	for _, tc := range cases {
		if err := MatchGuard(tc.status); !errors.Is(err, tc.wantErr) {
			t.Errorf("MatchGuard(%q) = %v, want %v", tc.status, err, tc.wantErr)
		}
	}
	if ErrAlreadyMatched.Error() != "inv_already_matched" {
		t.Fatalf("sentinel = %q, want inv_already_matched", ErrAlreadyMatched.Error())
	}
}

// The #41 list-filter normalization and validation, table-driven: the three
// statuses pass (plus the empty filter), page/page_size clamp into range with
// the absurd-page rejection, and the query is trimmed + uppercased with LIKE
// wildcards escaped — the #38 purchaseorder.ValidateList twin.
func TestValidateList(t *testing.T) {
	for name, tc := range map[string]struct {
		in      ListFilter
		want    ListFilter
		wantErr error
	}{
		"defaults": {ListFilter{}, ListFilter{Page: 1, PageSize: DefaultPageSize, Query: ""}, nil},
		"all three statuses": {ListFilter{Status: StatusPending}, ListFilter{Status: StatusPending, Page: 1, PageSize: DefaultPageSize}, nil},
		"matched":            {ListFilter{Status: StatusMatched}, ListFilter{Status: StatusMatched, Page: 1, PageSize: DefaultPageSize}, nil},
		"discrepancy":        {ListFilter{Status: StatusDiscrepancy}, ListFilter{Status: StatusDiscrepancy, Page: 1, PageSize: DefaultPageSize}, nil},
		"status trims": {ListFilter{Status: " pending "},
			ListFilter{Status: StatusPending, Page: 1, PageSize: DefaultPageSize}, nil},
		"unknown status":  {ListFilter{Status: "paused"}, ListFilter{}, ErrStatusInvalid},
		"po status leaks": {ListFilter{Status: "draft"}, ListFilter{}, ErrStatusInvalid},
		"page below one": {ListFilter{Page: -3, PageSize: 10},
			ListFilter{Page: 1, PageSize: 10}, nil},
		"page over max": {ListFilter{Page: MaxPage + 1}, ListFilter{Page: MaxPage + 1}, ErrPageInvalid},
		"page at max": {ListFilter{Page: MaxPage, PageSize: 10},
			ListFilter{Page: MaxPage, PageSize: 10}, nil},
		"page size below one": {ListFilter{PageSize: 0},
			ListFilter{Page: 1, PageSize: DefaultPageSize}, nil},
		"page size over max": {ListFilter{PageSize: 999},
			ListFilter{Page: 1, PageSize: MaxPageSize}, nil},
		"query trims and uppercases": {ListFilter{Query: "  inv/2026.001  "},
			ListFilter{Page: 1, PageSize: DefaultPageSize, Query: "INV/2026.001"}, nil},
		"query escapes wildcards": {ListFilter{Query: `50%_off\`},
			ListFilter{Page: 1, PageSize: DefaultPageSize, Query: `50\%\_OFF\`}, nil},
	} {
		got, err := ValidateList(tc.in)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: ValidateList(%+v) error = %v, want %v", name, tc.in, err, tc.wantErr)
			continue
		}
		if tc.wantErr != nil {
			continue
		}
		if got != tc.want {
			t.Errorf("%s: ValidateList(%+v) = %+v, want %+v", name, tc.in, got, tc.want)
		}
	}
}
