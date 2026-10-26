// Package purchaseorder implements the purchase-order aggregate as #35
// creates it: a draft PO converted from one approved requisition, lines
// copied exactly once, the total recomputed server-side, and the PO-%06d
// number allocated per tenant by the store. Pure domain — no IO lives here;
// the store (store.go) owns the conversion transaction, the per-tenant
// counter, and the single po.created outbox event committed with the insert.
// The lifecycle transitions out of 'draft' (issue → acknowledged → … →
// cancelled, FR-06) belong to #36; this package pins the seven status names
// and creates 'draft' only.
package purchaseorder

import (
	"errors"
	"strings"
	"time"
)

// Purchase-order lifecycle statuses (PRD FR-06). #35 creates StatusDraft
// only; the transitions and their events are #36's — the constants (and the
// migration 0007 CHECK) exist from day one so #36 never needs a schema
// change.
const (
	StatusDraft             = "draft"
	StatusIssued            = "issued"
	StatusAcknowledged      = "acknowledged"
	StatusPartiallyReceived = "partially_received"
	StatusReceived          = "received"
	StatusClosed            = "closed"
	StatusCancelled         = "cancelled"
)

// Currency allowlist mirrors the requisition domain (FR-03): a PO shares its
// requisition's currency, so the same two values are legal here, in the
// domain, and in the 0007 CHECK.
const (
	CurrencyIDR = "IDR"
	CurrencyUSD = "USD"
)

var allowedCurrencies = map[string]bool{
	CurrencyIDR: true,
	CurrencyUSD: true,
}

// CurrencyAllowed reports whether c is on the allowlist. Matching is exact.
func CurrencyAllowed(c string) bool { return allowedCurrencies[c] }

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged (same contract as the vendor/requisition
// domains).
var (
	ErrVendorIDInvalid        = errors.New("po_vendor_id_invalid")
	ErrRequisitionNotApproved = errors.New("po_requisition_not_approved")
	ErrItemsEmpty             = errors.New("po_items_empty")
	ErrDescriptionInvalid     = errors.New("po_description_invalid")
	ErrQtyInvalid             = errors.New("po_qty_invalid")
	ErrUnitPriceInvalid       = errors.New("po_unit_price_invalid")
	ErrCurrencyInvalid        = errors.New("po_currency_invalid")
)

// Item is one purchase-order line, copied verbatim from a requisition line
// on conversion. QtyReceived starts at 0; goods receipts (#37) advance it.
type Item struct {
	Description    string
	Qty            int32
	QtyReceived    int32
	UnitPriceCents int64
}

// LineTotalCents returns qty × unit price for the line.
func (i Item) LineTotalCents() int64 { return int64(i.Qty) * i.UnitPriceCents }

// PurchaseOrder is the aggregate root as conversion creates it: status is
// always draft, issued_at is nil (#36 sets both), and the number was
// allocated from the tenant's po_counters sequence inside the conversion
// transaction.
type PurchaseOrder struct {
	ID            string
	TenantID      string
	Number        string
	RequisitionID string
	VendorID      string
	Status        string
	Currency      string
	TotalCents    int64
	Items         []Item
	IssuedAt      *time.Time
	CreatedAt     time.Time
	CreatedBy     string
	UpdatedAt     time.Time
	UpdatedBy     string
}

// NewInput is the conversion payload: the requisition's copied fields plus
// the freshly allocated number. The store gates the requisition status
// (approved) and the vendor (active, tx-scoped) before calling New; the
// constructor re-validates the copied shape — belt and braces.
type NewInput struct {
	TenantID      string
	Number        string
	RequisitionID string
	VendorID      string
	Currency      string
	Items         []Item
	Now           time.Time
	CreatedBy     string
}

// New builds the draft PO (FR-05): the vendor must be named (requisitions
// carry no vendor), the lines are copied exactly once, and the total is
// recomputed from the copied lines — equal to the source requisition's total
// by construction, which the store asserts.
func New(in NewInput) (*PurchaseOrder, error) {
	if strings.TrimSpace(in.VendorID) == "" {
		return nil, ErrVendorIDInvalid
	}
	if !CurrencyAllowed(in.Currency) {
		return nil, ErrCurrencyInvalid
	}
	if len(in.Items) == 0 {
		return nil, ErrItemsEmpty
	}
	for _, it := range in.Items {
		if strings.TrimSpace(it.Description) == "" {
			return nil, ErrDescriptionInvalid
		}
		if it.Qty <= 0 {
			return nil, ErrQtyInvalid
		}
		if it.UnitPriceCents < 0 {
			return nil, ErrUnitPriceInvalid
		}
	}
	return &PurchaseOrder{
		TenantID:      in.TenantID,
		Number:        in.Number,
		RequisitionID: in.RequisitionID,
		VendorID:      strings.TrimSpace(in.VendorID),
		Status:        StatusDraft,
		Currency:      in.Currency,
		TotalCents:    totalCents(in.Items),
		Items:         append([]Item(nil), in.Items...),
		CreatedAt:     in.Now,
		CreatedBy:     in.CreatedBy,
		UpdatedAt:     in.Now,
		UpdatedBy:     in.CreatedBy,
	}, nil
}

func totalCents(items []Item) int64 {
	var total int64
	for _, it := range items {
		total += it.LineTotalCents()
	}
	return total
}
