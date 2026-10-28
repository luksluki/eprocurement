package match

import (
	"errors"
	"testing"
)

// poItem and invLine are the table fixtures' shorthand builders.
func poItem(id string, ordered, received int32, price int64) POItem {
	return POItem{ID: id, QtyOrdered: ordered, QtyReceived: received, UnitPriceCents: price}
}

func invLine(id string, qty int32, price int64, cum int32) Line {
	return Line{POItemID: id, Qty: qty, UnitPriceCents: price, QtyInvoicedCum: cum}
}

func TestValidateSettings(t *testing.T) {
	cases := []struct {
		name    string
		in      Settings
		wantErr error
	}{
		{"zero zero is the strict default", Settings{0, 0}, nil},
		{"both at the upper bound", Settings{100, 100}, nil},
		{"typical tolerance", Settings{5, 2}, nil},
		{"negative qty pct", Settings{-1, 0}, ErrQtyPctInvalid},
		{"qty pct over 100", Settings{101, 0}, ErrQtyPctInvalid},
		{"negative price pct", Settings{0, -5}, ErrPricePctInvalid},
		{"price pct over 100", Settings{0, 101}, ErrPricePctInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSettings(tc.in)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateSettings(%+v) = %v, want nil", tc.in, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateSettings(%+v) = %v, want %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// TestRun is the engine's decision table. qtyInvoicedCum is always the
// cumulative sum over the PO's invoices including this one — the store's
// snapshot; the engine itself treats it as one number.
func TestRun(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Result
	}{
		{
			name: "full receipt full invoice matches",
			in: Input{
				POItems:  []POItem{poItem("po-1", 10, 10, 500)},
				Lines:    []Line{invLine("po-1", 10, 500, 10)},
				Settings: Settings{0, 0},
			},
			want: Result{Matched: true, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 10, QtyReceived: 10, QtyInvoiced: 10,
				POUnitPriceCents: 500, InvoiceUnitPriceCents: 500,
			}}},
		},
		{
			name: "partial receipt, partial invoice within strict zero tolerance",
			in: Input{
				POItems:  []POItem{poItem("po-1", 10, 6, 500)},
				Lines:    []Line{invLine("po-1", 6, 500, 6)},
				Settings: Settings{0, 0},
			},
			want: Result{Matched: true, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 10, QtyReceived: 6, QtyInvoiced: 6,
				POUnitPriceCents: 500, InvoiceUnitPriceCents: 500,
			}}},
		},
		{
			name: "progress billing: invoice 30 of received 60 is NOT a breach",
			in: Input{
				POItems:  []POItem{poItem("po-1", 100, 60, 1000)},
				Lines:    []Line{invLine("po-1", 30, 1000, 30)},
				Settings: Settings{0, 0},
			},
			want: Result{Matched: true, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 100, QtyReceived: 60, QtyInvoiced: 30,
				POUnitPriceCents: 1000, InvoiceUnitPriceCents: 1000,
			}}},
		},
		{
			name: "billed line with no receipt at all is a qty breach",
			in: Input{
				POItems:  []POItem{poItem("po-1", 10, 0, 500)},
				Lines:    []Line{invLine("po-1", 5, 500, 5)},
				Settings: Settings{10, 0},
			},
			want: Result{Matched: false, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 10, QtyReceived: 0, QtyInvoiced: 5,
				POUnitPriceCents: 500, InvoiceUnitPriceCents: 500, QtyBreach: true,
				Reason: "inv_over_bill_qty: po_item=po-1 invoiced=5 received=0 pct=10",
			}}},
		},
		{
			name: "cumulative two invoices 40+40 vs received 60 breach at zero tolerance",
			in: Input{
				POItems:  []POItem{poItem("po-1", 100, 60, 1000)},
				Lines:    []Line{invLine("po-1", 40, 1000, 80)}, // this one 40, the PO's total 80
				Settings: Settings{0, 0},
			},
			want: Result{Matched: false, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 100, QtyReceived: 60, QtyInvoiced: 80,
				POUnitPriceCents: 1000, InvoiceUnitPriceCents: 1000, QtyBreach: true,
				Reason: "inv_over_bill_qty: po_item=po-1 invoiced=80 received=60 pct=0",
			}}},
		},
		{
			name: "cumulative boundary equality is within (80 == 60 x 1.333... not used; exact 33pct case below)",
			in: Input{
				POItems:  []POItem{poItem("po-1", 100, 60, 1000)},
				Lines:    []Line{invLine("po-1", 40, 1000, 80)},
				Settings: Settings{34, 0}, // 60*134 = 8040 >= 80*100 = 8000 → within
			},
			want: Result{Matched: true, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 100, QtyReceived: 60, QtyInvoiced: 80,
				POUnitPriceCents: 1000, InvoiceUnitPriceCents: 1000,
			}}},
		},
		{
			name: "qty boundary exactly at tolerance is within, one above is a breach",
			in: Input{
				POItems:  []POItem{poItem("po-1", 100, 50, 1000)},
				Lines:    []Line{invLine("po-1", 55, 1000, 55)}, // 55 == 50 x 1.10 exactly
				Settings: Settings{10, 0},
			},
			want: Result{Matched: true, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 100, QtyReceived: 50, QtyInvoiced: 55,
				POUnitPriceCents: 1000, InvoiceUnitPriceCents: 1000,
			}}},
		},
		{
			name: "price above PO beyond tolerance is a breach",
			in: Input{
				POItems:  []POItem{poItem("po-1", 10, 10, 1000)},
				Lines:    []Line{invLine("po-1", 10, 1150, 10)}, // +15% > 10%
				Settings: Settings{0, 10},
			},
			want: Result{Matched: false, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 10, QtyReceived: 10, QtyInvoiced: 10,
				POUnitPriceCents: 1000, InvoiceUnitPriceCents: 1150, PriceBreach: true,
				Reason: "inv_over_bill_price: po_item=po-1 po_price=1000 invoice_price=1150 pct=10",
			}}},
		},
		{
			name: "price below PO beyond tolerance is a breach too (symmetric)",
			in: Input{
				POItems:  []POItem{poItem("po-1", 10, 10, 1000)},
				Lines:    []Line{invLine("po-1", 10, 850, 10)}, // -15%
				Settings: Settings{0, 10},
			},
			want: Result{Matched: false, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 10, QtyReceived: 10, QtyInvoiced: 10,
				POUnitPriceCents: 1000, InvoiceUnitPriceCents: 850, PriceBreach: true,
				Reason: "inv_over_bill_price: po_item=po-1 po_price=1000 invoice_price=850 pct=10",
			}}},
		},
		{
			name: "price boundary exactly at tolerance is within (110 == 1000 x 11%)",
			in: Input{
				POItems:  []POItem{poItem("po-1", 10, 10, 1000)},
				Lines:    []Line{invLine("po-1", 10, 1100, 10)},
				Settings: Settings{0, 10},
			},
			want: Result{Matched: true, Lines: []ResultLine{{
				POItemID: "po-1", QtyOrdered: 10, QtyReceived: 10, QtyInvoiced: 10,
				POUnitPriceCents: 1000, InvoiceUnitPriceCents: 1100,
			}}},
		},
		{
			name: "zero PO price: zero invoice price matches, positive is a breach",
			in: Input{
				POItems:  []POItem{poItem("free", 5, 5, 0), poItem("paid", 5, 5, 100)},
				Lines:    []Line{invLine("free", 5, 0, 5), invLine("paid", 5, 1, 5)},
				Settings: Settings{0, 50},
			},
			want: Result{Matched: false, Lines: []ResultLine{
				{
					POItemID: "free", QtyOrdered: 5, QtyReceived: 5, QtyInvoiced: 5,
					POUnitPriceCents: 0, InvoiceUnitPriceCents: 0,
				},
				{
					POItemID: "paid", QtyOrdered: 5, QtyReceived: 5, QtyInvoiced: 5,
					POUnitPriceCents: 100, InvoiceUnitPriceCents: 1, PriceBreach: true,
					Reason: "inv_over_bill_price: po_item=paid po_price=100 invoice_price=1 pct=50",
				},
			}},
		},
		{
			name: "one breach among clean lines flags the whole invoice",
			in: Input{
				POItems:  []POItem{poItem("a", 10, 10, 100), poItem("b", 10, 4, 100)},
				Lines:    []Line{invLine("a", 10, 100, 10), invLine("b", 10, 100, 10)},
				Settings: Settings{0, 0},
			},
			want: Result{Matched: false, Lines: []ResultLine{
				{
					POItemID: "a", QtyOrdered: 10, QtyReceived: 10, QtyInvoiced: 10,
					POUnitPriceCents: 100, InvoiceUnitPriceCents: 100,
				},
				{
					POItemID: "b", QtyOrdered: 10, QtyReceived: 4, QtyInvoiced: 10, QtyBreach: true,
					POUnitPriceCents: 100, InvoiceUnitPriceCents: 100,
					Reason: "inv_over_bill_qty: po_item=b invoiced=10 received=4 pct=0",
				},
			}},
		},
		{
			name: "unbilled PO line simply has no result line",
			in: Input{
				POItems:  []POItem{poItem("a", 10, 10, 100), poItem("idle", 10, 0, 100)},
				Lines:    []Line{invLine("a", 10, 100, 10)},
				Settings: Settings{0, 0},
			},
			want: Result{Matched: true, Lines: []ResultLine{{
				POItemID: "a", QtyOrdered: 10, QtyReceived: 10, QtyInvoiced: 10,
				POUnitPriceCents: 100, InvoiceUnitPriceCents: 100,
			}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Run(tc.in)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.Matched != tc.want.Matched || len(got.Lines) != len(tc.want.Lines) {
				t.Fatalf("Run = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want.Lines {
				if got.Lines[i] != tc.want.Lines[i] {
					t.Fatalf("line %d = %+v, want %+v", i, got.Lines[i], tc.want.Lines[i])
				}
			}
		})
	}
}

// AC (determinism): identical state produces byte-identical JSON, and the
// invoice's own line order never leaks into the result — lines follow the
// PO items' order.
func TestRunDeterministicJSON(t *testing.T) {
	base := Input{
		POItems: []POItem{
			poItem("po-1", 100, 60, 1000),
			poItem("po-2", 50, 25, 250),
		},
		Lines: []Line{
			invLine("po-1", 70, 1000, 70), // qty breach at 0%
			invLine("po-2", 25, 300, 25),  // price breach
		},
		Settings: Settings{0, 0},
	}
	first, err := Run(base)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	shuffled := base
	shuffled.Lines = []Line{ // reversed input order, same state
		invLine("po-2", 25, 300, 25),
		invLine("po-1", 70, 1000, 70),
	}
	second, err := Run(shuffled)
	if err != nil {
		t.Fatalf("run shuffled: %v", err)
	}
	a, b := first.ResultJSON(), second.ResultJSON()
	if a != b {
		t.Fatalf("JSON not deterministic:\n%s\n%s", a, b)
	}
	if first.Matched {
		t.Fatalf("two breaches must flag the invoice")
	}
	again, err := Run(base)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again.ResultJSON() != a {
		t.Fatalf("same input re-run produced different bytes:\n%s\n%s", again.ResultJSON(), a)
	}
	// The pinned shape: matched flag first, lines in PO order, integers only.
	want := `{"matched":false,"lines":[` +
		`{"po_item_id":"po-1","qty_ordered":100,"qty_received":60,"qty_invoiced":70,` +
		`"po_unit_price_cents":1000,"invoice_unit_price_cents":1000,` +
		`"qty_breach":true,"price_breach":false,` +
		`"reason":"inv_over_bill_qty: po_item=po-1 invoiced=70 received=60 pct=0"},` +
		`{"po_item_id":"po-2","qty_ordered":50,"qty_received":25,"qty_invoiced":25,` +
		`"po_unit_price_cents":250,"invoice_unit_price_cents":300,` +
		`"qty_breach":false,"price_breach":true,` +
		`"reason":"inv_over_bill_price: po_item=po-2 po_price=250 invoice_price=300 pct=0"}]}`
	if a != want {
		t.Fatalf("JSON =\n%s\nwant\n%s", a, want)
	}
}

func TestRunUnknownPOItem(t *testing.T) {
	if _, err := Run(Input{
		POItems:  []POItem{poItem("po-1", 10, 10, 100)},
		Lines:    []Line{invLine("po-9", 1, 100, 1)},
		Settings: Settings{0, 0},
	}); !errors.Is(err, ErrPOItemUnknown) {
		t.Fatalf("err = %v, want ErrPOItemUnknown", err)
	}
}

// Integer cross-multiplication guards at values where float math would
// already have rounded: 1/3-style percentages never divide anything.
func TestBreachBoundaryArithmetic(t *testing.T) {
	cases := []struct {
		name                    string
		invoiced, received, pct int32
		poPrice, invPrice       int64
		pricePct                int32
		wantQty, wantPrice      bool
	}{
		{"80 vs 60 at 33pct: 8000 > 7980 breach", 80, 60, 33, 1000, 1000, 0, true, false},
		{"80 vs 60 at 34pct: 8000 <= 8040 within", 80, 60, 34, 1000, 1000, 0, false, false},
		{"55 vs 50 at 10pct: 5500 == 5500 within", 55, 50, 10, 1000, 1000, 0, false, false},
		{"56 vs 50 at 10pct: 5600 > 5500 breach", 56, 50, 10, 1000, 1000, 0, true, false},
		{"1001 vs 1000 at 0pct price: 100 > 0 breach", 10, 10, 0, 1000, 1001, 0, false, true},
		{"999 vs 1000 at 0pct price: 100 > 0 breach", 10, 10, 0, 1000, 999, 0, false, true},
		{"1005 vs 1000 at 1pct: 500 <= 1000 within", 10, 10, 0, 1000, 1005, 1, false, false},
		{"big values no overflow: qty 12pct within, price 10.5/9e9 beyond 10pct", 2_000_000_000, 1_800_000_000, 12, 9_000_000_000, 10_500_000_000, 10, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := qtyBreach(tc.invoiced, tc.received, tc.pct); got != tc.wantQty {
				t.Fatalf("qtyBreach(%d, %d, %d) = %v, want %v", tc.invoiced, tc.received, tc.pct, got, tc.wantQty)
			}
			if got := priceBreach(tc.poPrice, tc.invPrice, tc.pricePct); got != tc.wantPrice {
				t.Fatalf("priceBreach(%d, %d, %d) = %v, want %v", tc.poPrice, tc.invPrice, tc.pricePct, got, tc.wantPrice)
			}
		})
	}
}

// Sanity: the defaults really are the strict zero tolerance.
func TestDefaultSettings(t *testing.T) {
	if s := DefaultSettings(); s != (Settings{QtyPct: 0, PricePct: 0}) {
		t.Fatalf("DefaultSettings = %+v", s)
	}
	if err := ValidateSettings(DefaultSettings()); err != nil {
		t.Fatalf("default settings invalid: %v", err)
	}
}
