// Package requisition implements the purchase-requisition aggregate: the
// Money and Item value objects, the draft → … → closed state machine, and
// the in-memory domain events every state change records. Pure domain — no
// IO lives here; persistence (#29) and the transactional outbox (#45) adapt
// on top of this package.
package requisition

import (
	"errors"
	"strings"
	"time"
)

// Requisition lifecycle statuses (PRD FR-03/FR-04).
const (
	StatusDraft     = "draft"
	StatusSubmitted = "submitted"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
	StatusClosed    = "closed"
)

// Domain event types, one per state change plus creation. The event history
// (#43) and the outbox (#45) consume them later; this package only records
// them on the aggregate.
const (
	EventCreated   = "requisition_created"
	EventSubmitted = "requisition_submitted"
	EventApproved  = "requisition_approved"
	EventRejected  = "requisition_rejected"
	EventCancelled = "requisition_cancelled"
	EventClosed    = "requisition_closed"
)

// Currency allowlist (FR-03; maintainer decision 3 on issue #28). The list
// is code, so extending it never needs a migration beyond the CHECK
// constraint that mirrors it in migration 0003.
const (
	CurrencyIDR = "IDR"
	CurrencyUSD = "USD"
)

var allowedCurrencies = map[string]bool{
	CurrencyIDR: true,
	CurrencyUSD: true,
}

// CurrencyAllowed reports whether c is on the domain allowlist. Matching is
// exact: callers normalize (e.g. uppercase) before checking.
func CurrencyAllowed(c string) bool { return allowedCurrencies[c] }

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged (same contract as the vendor domain).
var (
	ErrCurrencyInvalid    = errors.New("requisition_currency_invalid")
	ErrDescriptionInvalid = errors.New("requisition_description_invalid")
	ErrQtyInvalid         = errors.New("requisition_qty_invalid")
	ErrUnitPriceInvalid   = errors.New("requisition_unit_price_invalid")
	ErrItemsEmpty         = errors.New("requisition_items_empty")
	ErrIllegalTransition  = errors.New("requisition_illegal_transition")
)

// Money is an amount as integer cents plus an allowlisted currency. Integer
// cents avoid float rounding; the currency lives on the requisition header,
// so every line of one requisition shares it.
type Money struct {
	AmountCents int64
	Currency    string
}

// Item is one requisition line. Prices are integer cents in the requisition's
// currency.
type Item struct {
	Description    string
	Qty            int32
	UnitPriceCents int64
	Category       string
}

// LineTotalCents returns qty × unit price for the line.
func (i Item) LineTotalCents() int64 { return int64(i.Qty) * i.UnitPriceCents }

// Event is a domain event recorded on the aggregate. Payload stays minimal;
// transport and enrichment are decided by #43/#45.
type Event struct {
	Type       string
	OccurredAt time.Time
	Actor      string
}

// transitionEvents is the complete legal edge set of the state machine.
// Loose cancel (maintainer decision 1 on issue #28): draft, submitted, and
// approved requisitions may all be cancelled. rejected, cancelled, and
// closed are terminal. approved → closed happens through PO conversion
// (#35); the edge is owned by Close now so it is testable.
var transitionEvents = map[[2]string]string{
	{StatusDraft, StatusSubmitted}:     EventSubmitted,
	{StatusDraft, StatusCancelled}:     EventCancelled,
	{StatusSubmitted, StatusApproved}:  EventApproved,
	{StatusSubmitted, StatusRejected}:  EventRejected,
	{StatusSubmitted, StatusCancelled}: EventCancelled,
	{StatusApproved, StatusCancelled}:  EventCancelled,
	{StatusApproved, StatusClosed}:     EventClosed,
}

// Terminal reports whether status has no outgoing edge.
func Terminal(status string) bool {
	return status == StatusRejected || status == StatusCancelled || status == StatusClosed
}

// Requisition is the aggregate root. Items are mutable only while the
// status is draft; after that every state change goes through the
// transition methods.
type Requisition struct {
	ID          string
	TenantID    string
	Number      string
	RequesterID string
	Status      string
	Currency    string
	TotalCents  int64
	Items       []Item
	SubmittedAt *time.Time
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
	UpdatedBy   string

	events []Event
}

// NewInput is the draft-creation payload. Items may be empty: a draft
// collects its lines before submit.
type NewInput struct {
	TenantID    string
	Number      string
	RequesterID string
	Currency    string
	Items       []Item
	Now         time.Time
	CreatedBy   string
}

// New creates a draft requisition and records a RequisitionCreated event.
func New(in NewInput) (*Requisition, error) {
	if !CurrencyAllowed(in.Currency) {
		return nil, ErrCurrencyInvalid
	}
	for _, it := range in.Items {
		if err := validateItem(it); err != nil {
			return nil, err
		}
	}
	return &Requisition{
		TenantID:    in.TenantID,
		Number:      in.Number,
		RequesterID: in.RequesterID,
		Status:      StatusDraft,
		Currency:    in.Currency,
		TotalCents:  totalCents(in.Items),
		Items:       append([]Item(nil), in.Items...),
		CreatedAt:   in.Now,
		CreatedBy:   in.CreatedBy,
		UpdatedAt:   in.Now,
		UpdatedBy:   in.CreatedBy,
		events:      []Event{{Type: EventCreated, OccurredAt: in.Now, Actor: in.CreatedBy}},
	}, nil
}

// Submit moves draft → submitted. A requisition must carry at least one
// valid line to enter approval; the basket is re-checked here so a caller
// mutating Items directly cannot submit garbage. Item problems are reported
// as validation errors (they only surface from draft — every other status
// fails the transition itself).
func (r *Requisition) Submit(now time.Time, actor string) error {
	if r.Status == StatusDraft {
		if len(r.Items) == 0 {
			return ErrItemsEmpty
		}
		for _, it := range r.Items {
			if err := validateItem(it); err != nil {
				return err
			}
		}
	}
	if err := r.transition(StatusSubmitted, now, actor); err != nil {
		return err
	}
	at := now
	r.SubmittedAt = &at
	return nil
}

// Approve moves submitted → approved; the approval engine (#30/#31) calls it.
func (r *Requisition) Approve(now time.Time, actor string) error {
	return r.transition(StatusApproved, now, actor)
}

// Reject moves submitted → rejected; rejected is terminal.
func (r *Requisition) Reject(now time.Time, actor string) error {
	return r.transition(StatusRejected, now, actor)
}

// Cancel moves draft/submitted/approved → cancelled; cancelled is terminal.
func (r *Requisition) Cancel(now time.Time, actor string) error {
	return r.transition(StatusCancelled, now, actor)
}

// Close moves approved → closed. Requisitions close through PO conversion
// (#35); the method exists now so the edge is domain-owned from day one.
func (r *Requisition) Close(now time.Time, actor string) error {
	return r.transition(StatusClosed, now, actor)
}

// UpdateItems replaces the line items of a draft and recomputes the total —
// the create/edit API (#29) calls it. Non-draft requisitions are immutable.
// This is not a state-machine transition, so it records no event.
func (r *Requisition) UpdateItems(items []Item, now time.Time, actor string) error {
	if r.Status != StatusDraft {
		return ErrIllegalTransition
	}
	for _, it := range items {
		if err := validateItem(it); err != nil {
			return err
		}
	}
	r.Items = append([]Item(nil), items...)
	r.TotalCents = totalCents(items)
	r.UpdatedAt = now
	r.UpdatedBy = actor
	return nil
}

// Events returns a copy of the recorded events, oldest first.
func (r *Requisition) Events() []Event {
	return append([]Event(nil), r.events...)
}

// PullEvents returns the recorded events and clears the log — the outbox
// writer (#45) drains the aggregate after persisting.
func (r *Requisition) PullEvents() []Event {
	events := r.events
	r.events = nil
	return events
}

// Total returns the computed total as a Money value object.
func (r *Requisition) Total() Money {
	return Money{AmountCents: r.TotalCents, Currency: r.Currency}
}

// transition applies the current status → to edge if it is legal, appending
// exactly one domain event. Any other edge returns ErrIllegalTransition and
// leaves the aggregate untouched.
func (r *Requisition) transition(to string, now time.Time, actor string) error {
	event, ok := transitionEvents[[2]string{r.Status, to}]
	if !ok {
		return ErrIllegalTransition
	}
	r.Status = to
	r.UpdatedAt = now
	r.UpdatedBy = actor
	r.events = append(r.events, Event{Type: event, OccurredAt: now, Actor: actor})
	return nil
}

func validateItem(it Item) error {
	if strings.TrimSpace(it.Description) == "" {
		return ErrDescriptionInvalid
	}
	if it.Qty <= 0 {
		return ErrQtyInvalid
	}
	if it.UnitPriceCents < 0 {
		return ErrUnitPriceInvalid
	}
	return nil
}

func totalCents(items []Item) int64 {
	var total int64
	for _, it := range items {
		total += it.LineTotalCents()
	}
	return total
}
