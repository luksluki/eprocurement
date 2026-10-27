// Package goodsreceipt implements the goods-receipt note aggregate (#37,
// FR-07): one GRN records a partial or full delivery against one purchase
// order's lines. This file is the pure domain — request-shape validation and
// the aggregate shape; the store (store.go) owns the posting transaction
// (PO row lock, GRN-%06d allocation, inserts, the qty_received advances via
// purchaseorder.ApplyReceipt, and the grn.posted outbox row).
package goodsreceipt

import (
	"errors"
	"time"
)

// MaxNotesLen bounds the free-form notes field at the edge and the service
// layer — the approval reason's 1000-char convention.
const MaxNotesLen = 1000

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged (same contract as the vendor/requisition/
// purchase-order domains). The aggregate-relative guards (unknown line,
// cumulative over-receipt, status edge) live on the purchase-order aggregate
// as purchaseorder.Err* sentinels with grn_* codes; a malformed po_id
// reuses purchaseorder.ErrIDInvalid (po_id_invalid) — the path id names a
// PO, exactly like the lifecycle routes.
var (
	ErrItemsEmpty      = errors.New("grn_items_empty")
	ErrQtyInvalid      = errors.New("grn_qty_invalid")
	ErrPOItemDuplicate = errors.New("grn_po_item_duplicate")
	ErrNotesInvalid    = errors.New("grn_notes_invalid")
)

// Item is one receipt line: the PO line it receives against and the
// quantity received now.
type Item struct {
	POItemID    string
	QtyReceived int32
}

// GoodsReceipt is one goods-receipt note: who received what, when, against
// which PO. The number (GRN-%06d) is allocated per tenant inside the posting
// transaction; ReceivedAt is the receipt's own timestamp (= CreatedAt of the
// row on post).
type GoodsReceipt struct {
	ID         string
	TenantID   string
	POID       string
	Number     string
	Notes      string
	ReceivedBy string
	ReceivedAt time.Time
	CreatedAt  time.Time
	Items      []Item
}

// Validate checks the request shape before any transaction opens: at least
// one line, every quantity > 0 (partial receipts are cumulative — a zero or
// negative line is a client bug, not a no-op), no line twice (one GRN
// records each PO line at most once; further deliveries are further GRNs),
// and notes within the bound. Line membership (the po_item belongs to this
// PO) is checked by purchaseorder.ApplyReceipt against the locked aggregate.
func Validate(items []Item, notes string) error {
	if len(items) == 0 {
		return ErrItemsEmpty
	}
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if it.QtyReceived <= 0 {
			return ErrQtyInvalid
		}
		if seen[it.POItemID] {
			return ErrPOItemDuplicate
		}
		seen[it.POItemID] = true
	}
	if len(notes) > MaxNotesLen {
		return ErrNotesInvalid
	}
	return nil
}
