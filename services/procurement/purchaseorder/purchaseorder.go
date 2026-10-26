// Package purchaseorder implements the purchase-order aggregate: the draft
// PO #35 creates from one approved requisition (lines copied exactly once,
// total recomputed server-side, PO-%06d number allocated per tenant) and the
// #36 lifecycle state machine — seven statuses, ten legal edges, a receipt
// guard on cancel, and one in-memory domain event per legal transition. Pure
// domain — no IO lives here; the store (store.go) owns the conversion
// transaction, the guarded lifecycle UPDATEs, and the one outbox row per
// transition committed with them.
package purchaseorder

import (
	"errors"
	"strings"
	"time"
)

// Purchase-order lifecycle statuses (PRD FR-06). #35 creates StatusDraft
// only; #36's state machine moves the aggregate through the other six along
// the ten legal edges pinned in transitionEvents below (the migration 0007
// CHECK mirrors this set, so no schema change was needed).
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

// Domain event types recorded on the aggregate's in-memory log, one per
// lifecycle transition (the #28 requisition pattern). These are the
// aggregate-local names; the persisted record is the store's outbox rows
// (po.issued, po.acknowledged, po.cancelled, po.closed — the §14.6 names and
// their two-segment extensions). The receipt events land when #37 advances
// qty_received; the edges are domain-owned and tested here since #36.
const (
	EventIssued            = "po_issued"
	EventAcknowledged      = "po_acknowledged"
	EventPartiallyReceived = "po_partially_received"
	EventReceived          = "po_received"
	EventCancelled         = "po_cancelled"
	EventClosed            = "po_closed"
)

// Event is one domain event recorded on the aggregate (same shape as the
// requisition's): payload stays minimal, transport is the store's outbox.
type Event struct {
	Type       string
	OccurredAt time.Time
	Actor      string
}

// transitionEvents is the complete legal edge set of the FR-06 state machine:
// a linear happy path (draft → issued → acknowledged → partially_received →
// received → closed) plus a loose cancel (draft, issued, and acknowledged may
// all be cancelled, subject to the receipt guard in Cancel) and a short-close
// (partially_received → closed). closed and cancelled are terminal. The
// receipt edges (→ partially_received / received) are driven by goods
// receipts (#37); the methods exist now so the edges are domain-owned and
// testable from day one (the #28 Close precedent).
var transitionEvents = map[[2]string]string{
	{StatusDraft, StatusIssued}:                   EventIssued,
	{StatusDraft, StatusCancelled}:                EventCancelled,
	{StatusIssued, StatusAcknowledged}:            EventAcknowledged,
	{StatusIssued, StatusCancelled}:               EventCancelled,
	{StatusAcknowledged, StatusCancelled}:         EventCancelled,
	{StatusAcknowledged, StatusPartiallyReceived}: EventPartiallyReceived,
	{StatusAcknowledged, StatusReceived}:          EventReceived,
	{StatusPartiallyReceived, StatusReceived}:     EventReceived,
	{StatusPartiallyReceived, StatusClosed}:       EventClosed,
	{StatusReceived, StatusClosed}:                EventClosed,
}

// Terminal reports whether status has no outgoing edge.
func Terminal(status string) bool {
	return status == StatusClosed || status == StatusCancelled
}

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
	ErrIllegalTransition      = errors.New("po_illegal_transition")
	ErrHasReceipts            = errors.New("po_has_receipts")
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

// PurchaseOrder is the aggregate root: conversion (#35) creates the draft,
// the #36 lifecycle methods move it along the legal edges. Creation carries
// no in-memory event (the po.created outbox row is the creation record —
// dev-note 035); every lifecycle transition appends exactly one.
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

	events []Event
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

// totalQtyReceived sums qty_received over every line — the cancel guard's
// input. Any receipt at all (a single advanced line) pins the PO: the only
// way out of partially_received/received is Close.
func (p *PurchaseOrder) totalQtyReceived() int32 {
	var received int32
	for _, it := range p.Items {
		received += it.QtyReceived
	}
	return received
}

// Issue moves draft → issued and stamps issued_at — the moment the PO
// leaves the buyer and starts its SLA clock (PRD §16: acknowledgement
// reminders at > 3 days).
func (p *PurchaseOrder) Issue(now time.Time, actor string) error {
	if err := p.transition(StatusIssued, now, actor); err != nil {
		return err
	}
	at := now
	p.IssuedAt = &at
	return nil
}

// Acknowledge moves issued → acknowledged — the vendor's confirmation
// (po.acknowledged is a §14.6 catalog event).
func (p *PurchaseOrder) Acknowledge(now time.Time, actor string) error {
	return p.transition(StatusAcknowledged, now, actor)
}

// Cancel moves draft/issued/acknowledged → cancelled, but only while nothing
// has been received: once any qty_received > 0 the PO is pinned and the only
// exit is Close (short-close from partially_received). Cancelling a PO with
// receipts returns ErrHasReceipts; every other source status returns
// ErrIllegalTransition. Both guards run before the mutation, so a failed
// cancel leaves the aggregate and its event log untouched.
func (p *PurchaseOrder) Cancel(now time.Time, actor string) error {
	if _, ok := transitionEvents[[2]string{p.Status, StatusCancelled}]; !ok {
		return ErrIllegalTransition
	}
	if p.totalQtyReceived() > 0 {
		return ErrHasReceipts
	}
	return p.transition(StatusCancelled, now, actor)
}

// Close moves partially_received/received → closed — the terminal success
// state. partially_received → closed is the short-close (accepting the
// shortfall); received → closed is the normal completion.
func (p *PurchaseOrder) Close(now time.Time, actor string) error {
	return p.transition(StatusClosed, now, actor)
}

// MarkPartiallyReceived moves acknowledged → partially_received — the first
// partial goods receipt (#37 drives it; the edge is owned and tested here).
func (p *PurchaseOrder) MarkPartiallyReceived(now time.Time, actor string) error {
	return p.transition(StatusPartiallyReceived, now, actor)
}

// MarkReceived moves acknowledged/partially_received → received — the last
// line fully received (#37 drives it).
func (p *PurchaseOrder) MarkReceived(now time.Time, actor string) error {
	return p.transition(StatusReceived, now, actor)
}

// Events returns a copy of the recorded events, oldest first.
func (p *PurchaseOrder) Events() []Event {
	return append([]Event(nil), p.events...)
}

// PullEvents returns the recorded events and clears the log — the outbox
// writer drains the aggregate after persisting (same contract as the
// requisition's).
func (p *PurchaseOrder) PullEvents() []Event {
	events := p.events
	p.events = nil
	return events
}

// transition applies the current status → to edge if it is legal, appending
// exactly one domain event. Any other edge returns ErrIllegalTransition and
// leaves the aggregate untouched.
func (p *PurchaseOrder) transition(to string, now time.Time, actor string) error {
	event, ok := transitionEvents[[2]string{p.Status, to}]
	if !ok {
		return ErrIllegalTransition
	}
	p.Status = to
	p.UpdatedAt = now
	p.UpdatedBy = actor
	p.events = append(p.events, Event{Type: event, OccurredAt: now, Actor: actor})
	return nil
}
