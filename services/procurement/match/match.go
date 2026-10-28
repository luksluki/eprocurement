// Package match implements the three-way match engine (#40, FR-08): PO vs
// goods receipt vs vendor invoice, with per-tenant quantity and price
// tolerances. This file is the pure engine — no IO, no clock: the store
// (store.go) loads the locked state, this package decides, the store
// persists the verdict and its event. All arithmetic is integer
// cross-multiplication (no float), so the verdict and its stored JSON are
// deterministic: the same state always produces byte-identical output.
package match

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged (the invoice domain's contract).
var (
	// ErrQtyPctInvalid / ErrPricePctInvalid bound the tenant tolerances
	// (0..100, whole percentages).
	ErrQtyPctInvalid   = errors.New("match_qty_pct_invalid")
	ErrPricePctInvalid = errors.New("match_price_pct_invalid")
	// ErrPOItemUnknown answers an invoice line that names no line of the
	// PO — unreachable through the store (create enforces membership), the
	// engine's defensive shape guard.
	ErrPOItemUnknown = errors.New("inv_po_item_unknown")
)

// Settings are the tenant's tolerances: whole percentages, 0..100 each.
// The qty tolerance is upper-side only (over-invoicing); the price
// tolerance is symmetric (any deviation from the PO price).
type Settings struct {
	QtyPct   int32
	PricePct int32
}

// DefaultSettings is the effective configuration while a tenant has no
// match_settings row: 0%/0% — strict.
func DefaultSettings() Settings { return Settings{QtyPct: 0, PricePct: 0} }

// ValidateSettings checks the 0..100 bounds (whole percentages; negative or
// >100 answers the field's machine code).
func ValidateSettings(s Settings) error {
	if s.QtyPct < 0 || s.QtyPct > 100 {
		return ErrQtyPctInvalid
	}
	if s.PricePct < 0 || s.PricePct > 100 {
		return ErrPricePctInvalid
	}
	return nil
}

// POItem is one purchase-order line as the engine sees it: the ordered and
// received quantities and the PO unit price. Items arrive ordered by
// (created_at, id) — the store's load order — and the result keeps that
// order, which is what makes the stored JSON stable.
type POItem struct {
	ID             string
	QtyOrdered     int32
	QtyReceived    int32
	UnitPriceCents int64
}

// Line is one line of the invoice being matched: the PO line it bills, this
// invoice's quantity (already counted in QtyInvoicedCum), and this
// invoice's unit price.
type Line struct {
	POItemID       string
	Qty            int32
	UnitPriceCents int64
	QtyInvoicedCum int32 // cumulative over ALL the PO's invoices, this one included
}

// Input is everything the engine needs; the store assembles it inside the
// match transaction under the PO row lock, so the values are one consistent
// snapshot.
type Input struct {
	POItems  []POItem // ordered (created_at, id)
	Lines    []Line   // the invoice's own lines, any order
	Settings Settings
}

// ResultLine is one invoice line's verdict: the quantities the decision
// compared (qty_received is the PO line's current cumulative receipt,
// qty_invoiced the PO's cumulative invoiced total including this invoice)
// and the two unit prices. Reason carries the first violated rule as a
// pinned detail line — the machine code before the first colon, then the
// numbers — or "" when the line matched.
type ResultLine struct {
	POItemID              string `json:"po_item_id"`
	QtyOrdered            int32  `json:"qty_ordered"`
	QtyReceived           int32  `json:"qty_received"`
	QtyInvoiced           int32  `json:"qty_invoiced"`
	POUnitPriceCents      int64  `json:"po_unit_price_cents"`
	InvoiceUnitPriceCents int64  `json:"invoice_unit_price_cents"`
	QtyBreach             bool   `json:"qty_breach"`
	PriceBreach           bool   `json:"price_breach"`
	Reason                string `json:"reason"`
}

// Result is the whole verdict. Lines are ordered by the PO items'
// (created_at, id) — deterministic regardless of the invoice's line order.
// JSON is the only serialization: the store persists ResultJSON verbatim in
// invoices.match_result.
type Result struct {
	Matched bool         `json:"matched"`
	Lines   []ResultLine `json:"lines"`
}

// ResultJSON marshals the result deterministically: struct field order is
// fixed and every value is an integer or boolean, so identical state yields
// byte-identical JSON (the AC's determinism test).
func (r Result) ResultJSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		// Result holds only ints, bools, and strings — marshal cannot fail.
		return fmt.Sprintf(`{"matched":%v,"lines":null}`, r.Matched)
	}
	return string(b)
}

// qtyBreach is the cumulative quantity guard, upper-side only (the agent
// decision of 2026-10-28 on the maintainer's cumulative basis): the PO's
// invoiced total for the line — every invoice, this one included — may
// exceed the received quantity by at most pct%. Integer
// cross-multiplication: invoiced×100 > received×(100+pct) is a breach;
// exact equality sits inside the tolerance (the boundary is inclusive).
// Under-invoicing (progress billing: invoiced < received) is never a
// breach. A billed line with no receipt at all (received = 0, invoiced > 0)
// is always a breach.
func qtyBreach(invoiced, received int32, pct int32) bool {
	return int64(invoiced)*100 > int64(received)*int64(100+pct)
}

// priceBreach is the symmetric price guard: this invoice's unit price may
// deviate from the PO price by at most pct% in either direction.
// |Δ|×100 > po×pct is a breach; exact equality sits inside. A zero PO
// price (a free line) matches only a zero invoice price — any positive
// invoice price is a breach.
func priceBreach(poPrice, invoicePrice int64, pct int32) bool {
	if poPrice == 0 {
		return invoicePrice > 0
	}
	diff := invoicePrice - poPrice
	if diff < 0 {
		diff = -diff
	}
	return diff*100 > poPrice*int64(pct)
}

// Run decides the three-way match for one invoice. Each of the invoice's
// lines is checked against its PO line; the result lines follow the PO
// items' order. The invoice matches only when every line passed both
// guards. An input line naming an unknown PO item answers
// ErrPOItemUnknown — the create API's membership guard makes that
// unreachable through the store, so the engine's contract stays total.
func Run(in Input) (Result, error) {
	byID := make(map[string]POItem, len(in.POItems))
	for _, it := range in.POItems {
		byID[it.ID] = it
	}
	for _, l := range in.Lines {
		if _, ok := byID[l.POItemID]; !ok {
			return Result{}, ErrPOItemUnknown
		}
	}
	result := Result{Matched: true, Lines: make([]ResultLine, 0, len(in.Lines))}
	for _, po := range in.POItems {
		line, billed := lookupLine(in.Lines, po.ID)
		if !billed {
			continue // the invoice does not bill this PO line — nothing to check
		}
		out := ResultLine{
			POItemID:              po.ID,
			QtyOrdered:            po.QtyOrdered,
			QtyReceived:           po.QtyReceived,
			QtyInvoiced:           line.QtyInvoicedCum,
			POUnitPriceCents:      po.UnitPriceCents,
			InvoiceUnitPriceCents: line.UnitPriceCents,
		}
		switch {
		case qtyBreach(line.QtyInvoicedCum, po.QtyReceived, in.Settings.QtyPct):
			out.QtyBreach = true
			out.Reason = fmt.Sprintf("inv_over_bill_qty: po_item=%s invoiced=%d received=%d pct=%d",
				po.ID, line.QtyInvoicedCum, po.QtyReceived, in.Settings.QtyPct)
		case priceBreach(po.UnitPriceCents, line.UnitPriceCents, in.Settings.PricePct):
			out.PriceBreach = true
			out.Reason = fmt.Sprintf("inv_over_bill_price: po_item=%s po_price=%d invoice_price=%d pct=%d",
				po.ID, po.UnitPriceCents, line.UnitPriceCents, in.Settings.PricePct)
		}
		if out.QtyBreach || out.PriceBreach {
			result.Matched = false
		}
		result.Lines = append(result.Lines, out)
	}
	return result, nil
}

// lookupLine finds the invoice's line for one PO item. The create API
// guarantees at most one line per PO item per invoice, so the first hit is
// the only hit.
func lookupLine(lines []Line, poItemID string) (Line, bool) {
	for _, l := range lines {
		if l.POItemID == poItemID {
			return l, true
		}
	}
	return Line{}, false
}
