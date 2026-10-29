package purchaseorder

import (
	"errors"
	"testing"
	"time"
)

var convertItems = []Item{
	{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000},
	{Description: "Mouse", Qty: 3, UnitPriceCents: 150000},
}

// validInput is the conversion payload of an approved two-line requisition
// (2×7,500,000 + 3×150,000 = 15,450,000). Items are copied so the mutation
// table below never bleeds between cases.
func validInput() NewInput {
	return NewInput{
		TenantID:      "55555555-0000-0000-0000-000000000001",
		Number:        "PO-000001",
		RequisitionID: "55555555-0000-0000-0000-0000000000aa",
		VendorID:      "55555555-0000-0000-0000-0000000000bb",
		Currency:      "IDR",
		Items:         append([]Item(nil), convertItems...),
		Now:           time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC),
		CreatedBy:     "55555555-0000-0000-0000-0000000000cc",
	}
}

// AC: conversion creates a draft PO with the lines copied exactly once and
// the total recomputed server-side; issued_at stays nil (#36 owns issue).
func TestNewDraftFromRequisition(t *testing.T) {
	po, err := New(validInput())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if po.Status != StatusDraft {
		t.Fatalf("status = %s, want draft", po.Status)
	}
	if po.IssuedAt != nil {
		t.Fatalf("issued_at = %v, want nil while draft", po.IssuedAt)
	}
	if po.TotalCents != 15450000 {
		t.Fatalf("total = %d, want 15450000", po.TotalCents)
	}
	if len(po.Items) != 2 || po.Items[0].Description != "Laptop" || po.Items[1].Qty != 3 {
		t.Fatalf("items not copied verbatim: %+v", po.Items)
	}
	for _, it := range po.Items {
		if it.QtyReceived != 0 {
			t.Fatalf("qty_received = %d on creation, want 0", it.QtyReceived)
		}
	}
	if po.CreatedBy != po.UpdatedBy || po.CreatedAt != po.UpdatedAt {
		t.Fatalf("audit mismatch: %+v", po)
	}
}

// AC (failure paths): no vendor, bad currency, empty basket, and invalid
// lines each return their machine-code sentinel.
func TestNewValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*NewInput)
		want   error
	}{
		"no vendor":      {func(in *NewInput) { in.VendorID = " " }, ErrVendorIDInvalid},
		"bad currency":   {func(in *NewInput) { in.Currency = "EUR" }, ErrCurrencyInvalid},
		"no items":       {func(in *NewInput) { in.Items = nil }, ErrItemsEmpty},
		"blank desc":     {func(in *NewInput) { in.Items[0].Description = "  " }, ErrDescriptionInvalid},
		"zero qty":       {func(in *NewInput) { in.Items[0].Qty = 0 }, ErrQtyInvalid},
		"negative price": {func(in *NewInput) { in.Items[0].UnitPriceCents = -1 }, ErrUnitPriceInvalid},
	}
	for name, tc := range cases {
		in := validInput()
		tc.mutate(&in)
		if _, err := New(in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

var (
	poTestNow   = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	poTestActor = "55555555-0000-0000-0000-0000000000dd"
)

func newDraft(t *testing.T) *PurchaseOrder {
	t.Helper()
	po, err := New(validInput())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return po
}

// atStatus builds a PO parked in the given status through legal edges only.
func atStatus(t *testing.T, status string) *PurchaseOrder {
	t.Helper()
	steps := map[string][]string{
		StatusDraft:             {},
		StatusIssued:            {StatusIssued},
		StatusAcknowledged:      {StatusIssued, StatusAcknowledged},
		StatusPartiallyReceived: {StatusIssued, StatusAcknowledged, StatusPartiallyReceived},
		StatusReceived:          {StatusIssued, StatusAcknowledged, StatusReceived},
		StatusClosed:            {StatusIssued, StatusAcknowledged, StatusReceived, StatusClosed},
		StatusCancelled:         {StatusCancelled},
	}
	po := newDraft(t)
	for _, step := range steps[status] {
		if err := poActions[step].run(po); err != nil {
			t.Fatalf("build %s via %s: %v", status, step, err)
		}
	}
	return po
}

// poActions maps each transition's target status to the method that attempts
// it (the key doubles as the resulting status when the edge is legal).
type poAction struct {
	run func(*PurchaseOrder) error
}

var poActions = map[string]poAction{
	StatusIssued:            {func(p *PurchaseOrder) error { return p.Issue(poTestNow, poTestActor) }},
	StatusAcknowledged:      {func(p *PurchaseOrder) error { return p.Acknowledge(poTestNow, poTestActor) }},
	StatusPartiallyReceived: {func(p *PurchaseOrder) error { return p.MarkPartiallyReceived(poTestNow, poTestActor) }},
	StatusReceived:          {func(p *PurchaseOrder) error { return p.MarkReceived(poTestNow, poTestActor) }},
	StatusCancelled:         {func(p *PurchaseOrder) error { return p.Cancel(poTestNow, poTestActor) }},
	StatusClosed:            {func(p *PurchaseOrder) error { return p.Close(poTestNow, poTestActor) }},
}

var poAllStatuses = []string{
	StatusDraft, StatusIssued, StatusAcknowledged, StatusPartiallyReceived,
	StatusReceived, StatusClosed, StatusCancelled,
}

// poLegalEdges is the ten-edge FR-06 contract, mirrored against
// transitionEvents so the map cannot drift.
var poLegalEdges = [][2]string{
	{StatusDraft, StatusIssued},
	{StatusDraft, StatusCancelled},
	{StatusIssued, StatusAcknowledged},
	{StatusIssued, StatusCancelled},
	{StatusAcknowledged, StatusCancelled},
	{StatusAcknowledged, StatusPartiallyReceived},
	{StatusAcknowledged, StatusReceived},
	{StatusPartiallyReceived, StatusReceived},
	{StatusPartiallyReceived, StatusClosed},
	{StatusReceived, StatusClosed},
}

// AC: every legal transition lands in the target status, appends exactly one
// event of the matching type, and — on issue — stamps issued_at.
func TestLegalTransitions(t *testing.T) {
	if len(poLegalEdges) != len(transitionEvents) {
		t.Fatalf("legal set (%d) does not mirror transitionEvents (%d)", len(poLegalEdges), len(transitionEvents))
	}
	wantEvent := map[string]string{
		StatusIssued:            EventIssued,
		StatusAcknowledged:      EventAcknowledged,
		StatusPartiallyReceived: EventPartiallyReceived,
		StatusReceived:          EventReceived,
		StatusCancelled:         EventCancelled,
		StatusClosed:            EventClosed,
	}
	for _, edge := range poLegalEdges {
		from, to := edge[0], edge[1]
		t.Run(from+"->"+to, func(t *testing.T) {
			po := atStatus(t, from)
			before := len(po.Events())
			if err := poActions[to].run(po); err != nil {
				t.Fatalf("transition: %v", err)
			}
			if po.Status != to {
				t.Fatalf("status = %q, want %q", po.Status, to)
			}
			events := po.Events()
			if len(events) != before+1 {
				t.Fatalf("events = %d, want %d (exactly one append)", len(events), before+1)
			}
			if last := events[len(events)-1]; last.Type != wantEvent[to] || last.Actor != poTestActor {
				t.Fatalf("last event = %+v, want type %s actor %s", last, wantEvent[to], poTestActor)
			}
			if to == StatusIssued && (po.IssuedAt == nil || !po.IssuedAt.Equal(poTestNow)) {
				t.Fatalf("issuedAt = %v, want %v (stamped on issue)", po.IssuedAt, poTestNow)
			}
		})
	}
}

// AC: illegal transitions are rejected with ErrIllegalTransition, without
// moving the aggregate, stamping issued_at, or appending an event — every
// other cell of the 7×6 state×action matrix (self-loops, skips, and moves out
// of the terminal closed/cancelled included).
func TestIllegalTransitions(t *testing.T) {
	legal := map[[2]string]bool{}
	for _, e := range poLegalEdges {
		legal[e] = true
	}
	illegal := 0
	for _, from := range poAllStatuses {
		for target, act := range poActions {
			if legal[[2]string{from, target}] {
				continue
			}
			illegal++
			t.Run(from+"-"+target, func(t *testing.T) {
				po := atStatus(t, from)
				eventsBefore := po.Events()
				issuedBefore := po.IssuedAt
				err := act.run(po)
				if !errors.Is(err, ErrIllegalTransition) || err != ErrIllegalTransition {
					t.Fatalf("err = %v, want ErrIllegalTransition", err)
				}
				if po.Status != from {
					t.Fatalf("status = %q, illegal transition must not move it", po.Status)
				}
				if got := po.Events(); len(got) != len(eventsBefore) {
					t.Fatalf("events = %d, want %d (no append on failure)", len(got), len(eventsBefore))
				}
				if po.IssuedAt != issuedBefore {
					t.Fatalf("issuedAt moved on failure: %v → %v", issuedBefore, po.IssuedAt)
				}
			})
		}
	}
	if illegal != 32 {
		t.Fatalf("illegal cases = %d, want 32 (7 statuses × 6 actions − 10 legal)", illegal)
	}
}

// AC (cancel rules respect receipts): with any qty_received > 0 the cancel
// guard fires from every cancellable source status (ErrHasReceipts, nothing
// mutated, no event), while a post-receipt status keeps the plain
// illegal-transition answer — the edge check wins there.
func TestCancelReceiptGuard(t *testing.T) {
	for _, from := range []string{StatusDraft, StatusIssued, StatusAcknowledged} {
		t.Run(from, func(t *testing.T) {
			po := atStatus(t, from)
			po.Items[1].QtyReceived = 1 // one advanced line pins the PO
			eventsBefore := po.Events()
			if err := po.Cancel(poTestNow, poTestActor); !errors.Is(err, ErrHasReceipts) {
				t.Fatalf("cancel with receipts: err = %v, want ErrHasReceipts", err)
			}
			if po.Status != from || len(po.Events()) != len(eventsBefore) {
				t.Fatalf("failed guard mutated the aggregate: %s/%d", po.Status, len(po.Events()))
			}
		})
	}
	// Zero receipts across all lines is cancellable (the sum, not per-line,
	// is the guard).
	po := atStatus(t, StatusAcknowledged)
	if err := po.Cancel(poTestNow, poTestActor); err != nil {
		t.Fatalf("cancel without receipts: %v", err)
	}
	if po.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", po.Status)
	}
	// A post-receipt status has no cancel edge at all: the transition error,
	// not the receipt error.
	received := atStatus(t, StatusReceived)
	if err := received.Cancel(poTestNow, poTestActor); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("cancel from received: err = %v, want ErrIllegalTransition", err)
	}
}

// closed and cancelled are terminal; no edge leaves them.
func TestTerminalStatuses(t *testing.T) {
	for _, s := range poAllStatuses {
		want := s == StatusClosed || s == StatusCancelled
		if Terminal(s) != want {
			t.Errorf("Terminal(%q) mismatch", s)
		}
	}
	for edge := range transitionEvents {
		if Terminal(edge[0]) {
			t.Errorf("edge %v leaves a terminal status", edge)
		}
	}
}

// Events() returns a copy; PullEvents drains — same contracts as the
// requisition aggregate.
func TestEventLogSemantics(t *testing.T) {
	po := newDraft(t)
	if got := po.Events(); len(got) != 0 {
		t.Fatalf("draft creation records no in-memory event (outbox row is the record): %+v", got)
	}
	if err := po.Issue(poTestNow, poTestActor); err != nil {
		t.Fatalf("issue: %v", err)
	}
	events := po.Events()
	events[0].Type = "tampered"
	if po.Events()[0].Type != EventIssued {
		t.Fatalf("Events() leaked the internal slice")
	}
	pulled := po.PullEvents()
	if len(pulled) != 1 || pulled[0].Type != EventIssued {
		t.Fatalf("pulled = %+v, want one %s", pulled, EventIssued)
	}
	if got := po.Events(); len(got) != 0 {
		t.Fatalf("after pull, events = %+v", got)
	}
	if err := po.Acknowledge(poTestNow, poTestActor); err != nil {
		t.Fatalf("acknowledge after pull: %v", err)
	}
	if got := po.Events(); len(got) != 1 || got[0].Type != EventAcknowledged {
		t.Fatalf("events after re-use = %+v", got)
	}
}

// receiptFixture builds the two-line PO (2× Laptop, 3× Mouse) with stable
// line ids and preset received quantities, parked at status via legal edges
// only — #37's ApplyReceipt reads the quantities cumulatively. (atStatus
// cannot preset quantities, so the edge walk is repeated here.)
func receiptFixture(t *testing.T, status string, laptopReceived, mouseReceived int32) *PurchaseOrder {
	t.Helper()
	in := validInput()
	in.Items = []Item{
		{ID: "po-item-laptop", Description: "Laptop", Qty: 2, QtyReceived: laptopReceived, UnitPriceCents: 7500000},
		{ID: "po-item-mouse", Description: "Mouse", Qty: 3, QtyReceived: mouseReceived, UnitPriceCents: 150000},
	}
	po, err := New(in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	steps := map[string][]string{
		StatusIssued:            {StatusIssued},
		StatusAcknowledged:      {StatusIssued, StatusAcknowledged},
		StatusPartiallyReceived: {StatusIssued, StatusAcknowledged, StatusPartiallyReceived},
		StatusReceived:          {StatusIssued, StatusAcknowledged, StatusReceived},
		StatusClosed:            {StatusIssued, StatusAcknowledged, StatusReceived, StatusClosed},
		StatusCancelled:         {StatusCancelled},
	}
	for _, step := range steps[status] {
		if err := poActions[step].run(po); err != nil {
			t.Fatalf("build %s via %s: %v", status, step, err)
		}
	}
	return po
}

// AC (#37): ApplyReceipt picks the target status from the post-receipt
// quantities through the three pinned branches — first partial from
// acknowledged → partially_received (one event), full → received (one event,
// direct or completion), and a further partial on a partially_received PO
// stays same-status with NO transition, NO event, and NO audit-column move
// (the store writes no purchase_orders UPDATE on that branch).
func TestApplyReceiptBranches(t *testing.T) {
	cases := []struct {
		name       string
		from       string
		laptopGot  int32
		mouseGot   int32
		lines      []ReceiptLine
		wantStatus string
		wantEvent  string // "" = no event appended (same-status branch)
		wantLaptop int32
		wantMouse  int32
	}{
		{
			name: "first partial from acknowledged", from: StatusAcknowledged,
			lines:      []ReceiptLine{{POItemID: "po-item-laptop", Qty: 1}},
			wantStatus: StatusPartiallyReceived, wantEvent: EventPartiallyReceived,
			wantLaptop: 1, wantMouse: 0,
		},
		{
			name: "second partial stays partially_received", from: StatusPartiallyReceived,
			laptopGot:  1,
			lines:      []ReceiptLine{{POItemID: "po-item-mouse", Qty: 2}},
			wantStatus: StatusPartiallyReceived, wantEvent: "",
			wantLaptop: 1, wantMouse: 2,
		},
		{
			name: "full direct from acknowledged", from: StatusAcknowledged,
			lines: []ReceiptLine{
				{POItemID: "po-item-laptop", Qty: 2},
				{POItemID: "po-item-mouse", Qty: 3},
			},
			wantStatus: StatusReceived, wantEvent: EventReceived,
			wantLaptop: 2, wantMouse: 3,
		},
		{
			name: "completion from partially_received", from: StatusPartiallyReceived,
			laptopGot: 2, mouseGot: 1,
			lines:      []ReceiptLine{{POItemID: "po-item-mouse", Qty: 2}},
			wantStatus: StatusReceived, wantEvent: EventReceived,
			wantLaptop: 2, wantMouse: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			po := receiptFixture(t, tc.from, tc.laptopGot, tc.mouseGot)
			before := po.Events()
			updatedAt, updatedBy := po.UpdatedAt, po.UpdatedBy
			if err := po.ApplyReceipt(poTestNow, poTestActor, tc.lines); err != nil {
				t.Fatalf("apply receipt: %v", err)
			}
			if po.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", po.Status, tc.wantStatus)
			}
			if po.Items[0].QtyReceived != tc.wantLaptop || po.Items[1].QtyReceived != tc.wantMouse {
				t.Fatalf("qty_received = (%d, %d), want (%d, %d)",
					po.Items[0].QtyReceived, po.Items[1].QtyReceived, tc.wantLaptop, tc.wantMouse)
			}
			events := po.Events()
			if tc.wantEvent == "" {
				// Same-status branch: no event, and the purchase_orders audit
				// columns must not move (the store skips the UPDATE
				// entirely — the domain mirrors that by not stamping).
				if len(events) != len(before) {
					t.Fatalf("events = %d, want %d (same-status branch appends none)", len(events), len(before))
				}
				if po.UpdatedAt != updatedAt || po.UpdatedBy != updatedBy {
					t.Fatalf("audit moved on same-status branch: (%v, %s) -> (%v, %s)",
						updatedAt, updatedBy, po.UpdatedAt, po.UpdatedBy)
				}
				return
			}
			if len(events) != len(before)+1 {
				t.Fatalf("events = %d, want %d (exactly one append)", len(events), len(before)+1)
			}
			if last := events[len(events)-1]; last.Type != tc.wantEvent || last.Actor != poTestActor {
				t.Fatalf("last event = %+v, want type %s actor %s", last, tc.wantEvent, poTestActor)
			}
		})
	}
}

// AC (#37 failure paths): the receipt-line guards — cumulative over-receipt
// with the pinned detail line (first violating line in request order),
// unknown line, duplicate line — and the status edge each return their
// sentinel without mutating the aggregate. The cumulative guard runs before
// the edge guard: a receipt against a received PO answers over-receipt
// (every line is full), not illegal transition.
func TestApplyReceiptGuards(t *testing.T) {
	t.Run("over receipt detail format", func(t *testing.T) {
		po := receiptFixture(t, StatusAcknowledged, 1, 0)
		err := po.ApplyReceipt(poTestNow, poTestActor, []ReceiptLine{{POItemID: "po-item-laptop", Qty: 2}})
		want := "grn_over_receipt: po_item=po-item-laptop received=1 requested=2 ordered=2"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if !errors.Is(err, ErrOverReceipt) {
			t.Fatalf("errors.Is(err, ErrOverReceipt) = false for %v", err)
		}
		if po.Items[0].QtyReceived != 1 || po.Status != StatusAcknowledged || len(po.Events()) != 2 {
			t.Fatalf("aggregate mutated by a rejected receipt: %+v", po)
		}
	})
	t.Run("first violating line wins", func(t *testing.T) {
		po := receiptFixture(t, StatusAcknowledged, 1, 3)
		err := po.ApplyReceipt(poTestNow, poTestActor, []ReceiptLine{
			{POItemID: "po-item-laptop", Qty: 5},
			{POItemID: "po-item-mouse", Qty: 5},
		})
		want := "grn_over_receipt: po_item=po-item-laptop received=1 requested=5 ordered=2"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})
	t.Run("unknown line", func(t *testing.T) {
		po := receiptFixture(t, StatusAcknowledged, 0, 0)
		err := po.ApplyReceipt(poTestNow, poTestActor, []ReceiptLine{{POItemID: "po-item-ghost", Qty: 1}})
		if !errors.Is(err, ErrReceiptItemUnknown) || err != ErrReceiptItemUnknown {
			t.Fatalf("err = %v, want ErrReceiptItemUnknown", err)
		}
	})
	t.Run("duplicate line", func(t *testing.T) {
		po := receiptFixture(t, StatusAcknowledged, 0, 0)
		err := po.ApplyReceipt(poTestNow, poTestActor, []ReceiptLine{
			{POItemID: "po-item-laptop", Qty: 1},
			{POItemID: "po-item-laptop", Qty: 1},
		})
		if !errors.Is(err, ErrReceiptItemDuplicate) || err != ErrReceiptItemDuplicate {
			t.Fatalf("err = %v, want ErrReceiptItemDuplicate", err)
		}
	})
	t.Run("illegal status", func(t *testing.T) {
		for _, from := range []string{StatusDraft, StatusIssued, StatusClosed, StatusCancelled} {
			po := receiptFixture(t, from, 0, 0)
			err := po.ApplyReceipt(poTestNow, poTestActor, []ReceiptLine{{POItemID: "po-item-laptop", Qty: 1}})
			if !errors.Is(err, ErrIllegalTransition) || err != ErrIllegalTransition {
				t.Fatalf("%s: err = %v, want ErrIllegalTransition", from, err)
			}
			if po.Status != from || po.Items[0].QtyReceived != 0 {
				t.Fatalf("%s: aggregate mutated by a rejected receipt", from)
			}
		}
	})
	t.Run("cumulative guard before edge guard on a received PO", func(t *testing.T) {
		po := receiptFixture(t, StatusReceived, 2, 3)
		err := po.ApplyReceipt(poTestNow, poTestActor, []ReceiptLine{{POItemID: "po-item-laptop", Qty: 1}})
		var over *OverReceiptError
		if !errors.As(err, &over) {
			t.Fatalf("err = %v, want the over-receipt answer, not the illegal-transition one", err)
		}
	})
}

// ValidateList (#38, table-driven — the requisition TestValidateListErrors
// twin): every status of the seven-value whitelist passes, unknown statuses
// and absurd pages answer their machine-code sentinels, and the rest is
// normalized (defaults applied, query trimmed + uppercased, LIKE wildcards
// escaped).
func TestValidateList(t *testing.T) {
	t.Run("happy path accepts every status and normalizes", func(t *testing.T) {
		for _, status := range []string{
			StatusDraft, StatusIssued, StatusAcknowledged, StatusPartiallyReceived,
			StatusReceived, StatusClosed, StatusCancelled,
		} {
			f, err := ValidateList(ListFilter{Status: " " + status + " ", Query: "  po-0001  ", Page: 2, PageSize: 50})
			if err != nil {
				t.Fatalf("status %s: err = %v", status, err)
			}
			if f.Status != status {
				t.Fatalf("status %s normalized to %q", status, f.Status)
			}
		}
		f, err := ValidateList(ListFilter{Query: "  po-0001  "})
		if err != nil || f.Query != "PO-0001" {
			t.Fatalf("normalized query = %+v (%v)", f, err)
		}
		// LIKE wildcards are escaped so they match literally.
		f, err = ValidateList(ListFilter{Query: "PO-%100_"})
		if err != nil || f.Query != `PO-\%100\_` {
			t.Fatalf("escaped query = %q (%v)", f.Query, err)
		}
		// Defaults: page 1, page size 20; page_size caps at 100.
		f, err = ValidateList(ListFilter{PageSize: 500})
		if err != nil || f.Page != 1 || f.PageSize != MaxPageSize {
			t.Fatalf("normalized filter = %+v (%v)", f, err)
		}
		if f, err := ValidateList(ListFilter{}); err != nil || f.PageSize != DefaultPageSize {
			t.Fatalf("default page size = %+v (%v)", f, err)
		}
		// Page at the boundary passes, one above it fails.
		if _, err := ValidateList(ListFilter{Page: MaxPage}); err != nil {
			t.Fatalf("max page: err = %v", err)
		}
	})
	t.Run("failure paths", func(t *testing.T) {
		cases := map[string]struct {
			f    ListFilter
			want error
		}{
			"unknown status":     {ListFilter{Status: "paused"}, ErrStatusInvalid},
			"requisition status": {ListFilter{Status: "submitted"}, ErrStatusInvalid},
			"page too large":     {ListFilter{Page: MaxPage + 1}, ErrPageInvalid},
			"negative page":      {ListFilter{Page: -3, PageSize: 5}, nil}, // clamped to 1
		}
		for name, tc := range cases {
			f, err := ValidateList(tc.f)
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", name, err, tc.want)
			}
			if tc.want == nil && f.Page != 1 {
				t.Errorf("%s: page = %d, want clamped to 1", name, f.Page)
			}
		}
	})
}
