// Package invoice implements the vendor-invoice aggregate (#39, the model
// and create API of the invoice epic; the 3-way match is #40): one invoice
// is the vendor's bill against one purchase order's lines. This file is the
// pure domain — request-shape validation and the typed over-billing error;
// the store (store.go) owns the creation transaction (PO row lock, the
// server-side vendor/currency/total derivation, the inserts, and the
// cumulative over-billing guard's persistence half).
package invoice

import (
	"errors"
	"fmt"
	"time"
)

// Invoice statuses. Create always lands StatusPending; the match (#40,
// FR-08) is the only writer of matched/discrepancy, through the
// match_result column this issue adds but never fills.
const (
	StatusPending     = "pending"
	StatusMatched     = "matched"
	StatusDiscrepancy = "discrepancy"
)

// MaxNumberLen bounds the vendor invoice number after trimming — the
// vendor's own document number, free-form text (issue #39 technical note:
// non-empty, trimmed, length-bounded).
const MaxNumberLen = 100

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged (same contract as the vendor/requisition/
// goods-receipt domains). The aggregate-relative guards (unknown PO line,
// illegal PO status) live here as ErrPOItemUnknown and on the
// purchase-order aggregate as purchaseorder.ErrIllegalTransition
// (po_illegal_transition — the #37 reuse); a malformed po_id reuses
// purchaseorder.ErrIDInvalid (po_id_invalid).
var (
	ErrNumberInvalid    = errors.New("inv_number_invalid")
	ErrItemsEmpty       = errors.New("inv_items_empty")
	ErrQtyInvalid       = errors.New("inv_qty_invalid")
	ErrUnitPriceInvalid = errors.New("inv_unit_price_invalid")
	ErrPOItemDuplicate  = errors.New("inv_po_item_duplicate")
	ErrPOItemUnknown    = errors.New("inv_po_item_unknown")
	// ErrOverBill is the cumulative over-billing guard's sentinel (#39):
	// the store returns the typed *OverBillError (which unwraps to this) so
	// the gRPC message can carry the pinned detail line while errors.Is
	// keeps matching the bare sentinel — the purchaseorder.OverReceiptError
	// pattern of #37.
	ErrOverBill = errors.New("inv_over_bill")
	// ErrAlreadyMatched is the #40 match's re-run guard: an invoice whose
	// status is already matched rejects a re-run (409). pending and
	// discrepancy may be re-matched — a late GRN resolving a discrepancy is
	// the intended loop, and each run appends its own event (maintainer
	// decision 2026-10-28).
	ErrAlreadyMatched = errors.New("inv_already_matched")
)

// OverBillError names the PO whose existing invoices plus this request
// would exceed po.total_cents. Error() is the wire format pinned in
// docs/schema/procurement.md — the machine code as the strict prefix before
// the first colon, then the cumulative amounts — so the API boundary and the
// gateway's dedicated renderer can split code from detail without parsing
// anything but the colon (the OverReceiptError twin of #37).
type OverBillError struct {
	PONumber  string
	Invoiced  int64 // the PO's existing invoices' total_cents before this one
	Requested int64 // this invoice's server-computed total_cents
	Total     int64 // the PO's total_cents
}

func (e *OverBillError) Error() string {
	return fmt.Sprintf("inv_over_bill: po_number=%s invoiced=%d requested=%d total=%d",
		e.PONumber, e.Invoiced, e.Requested, e.Total)
}

// Unwrap makes errors.Is(err, ErrOverBill) match the typed error.
func (e *OverBillError) Unwrap() error { return ErrOverBill }

// Item is one invoice line: the PO line it bills, the invoiced quantity,
// and the invoiced unit price. The server recomputes the total from these —
// a client-sent total would be untrusted input.
type Item struct {
	POItemID       string
	Qty            int32
	UnitPriceCents int64
}

// LineTotalCents returns qty × unit price for the line.
func (i Item) LineTotalCents() int64 { return int64(i.Qty) * i.UnitPriceCents }

// TotalCents computes the invoice total server-side from the lines.
func TotalCents(items []Item) int64 {
	var total int64
	for _, it := range items {
		total += it.LineTotalCents()
	}
	return total
}

// Invoice is one vendor invoice: the vendor's bill against one PO's lines.
// VendorID and Currency are derived server-side from the locked PO (never
// client input — the plan's #39 contract); Number is the vendor's own
// document number, unique per (tenant, vendor); TotalCents is recomputed
// server-side from Items. Status starts pending; #40's match owns
// matched/discrepancy and the MatchResult payload.
type Invoice struct {
	ID          string
	TenantID    string
	POID        string
	VendorID    string
	Number      string
	Currency    string
	TotalCents  int64
	Status      string
	MatchResult string // raw JSON, empty while pending (#40 owns the content)
	Items       []Item
	CreatedBy   string
	CreatedAt   time.Time
}

// MatchGuard answers whether the invoice's status admits a match run
// (#40): matched is final (ErrAlreadyMatched); pending and discrepancy may
// be (re-)matched. The store re-checks it under the PO row lock and again
// in the guarded UPDATE, so the guard here is the fast pre-transaction
// answer.
func MatchGuard(status string) error {
	if status == StatusMatched {
		return ErrAlreadyMatched
	}
	return nil
}

// ValidateNumber checks the vendor invoice number's shape: non-empty after
// trimming, at most MaxNumberLen characters. Uniqueness per
// (tenant, vendor) is the store's UNIQUE index, not a shape rule.
func ValidateNumber(number string) error {
	if number == "" || len(number) > MaxNumberLen {
		return ErrNumberInvalid
	}
	return nil
}

// ValidateItems checks the line shape before any transaction opens: at
// least one line, every quantity > 0, every unit price >= 0, and no line
// twice (one invoice bills each PO line at most once; further billings are
// further invoices). Line membership (the po_item belongs to this PO) is
// checked by the store against the locked aggregate.
func ValidateItems(items []Item) error {
	if len(items) == 0 {
		return ErrItemsEmpty
	}
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if it.Qty <= 0 {
			return ErrQtyInvalid
		}
		if it.UnitPriceCents < 0 {
			return ErrUnitPriceInvalid
		}
		if seen[it.POItemID] {
			return ErrPOItemDuplicate
		}
		seen[it.POItemID] = true
	}
	return nil
}
