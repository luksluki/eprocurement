package requisition

import (
	"errors"
	"testing"
	"time"
)

var (
	testNow   = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	testActor = "dddddddd-0000-0000-0000-0000000000aa"
)

func validItems() []Item {
	return []Item{
		{Description: "Laptop 14 inch", Qty: 2, UnitPriceCents: 8500000, Category: "IT"},
		{Description: "USB-C dock", Qty: 2, UnitPriceCents: 1500000, Category: "IT"},
	}
}

func newDraft(t *testing.T) *Requisition {
	t.Helper()
	r, err := New(NewInput{
		TenantID:    "dddddddd-0000-0000-0000-000000000001",
		Number:      "PR-2026-0001",
		RequesterID: testActor,
		Currency:    CurrencyIDR,
		Items:       validItems(),
		Now:         testNow,
		CreatedBy:   testActor,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// atStatus builds a requisition parked in the given status through legal
// transitions only.
func atStatus(t *testing.T, status string) *Requisition {
	t.Helper()
	steps := map[string][]string{
		StatusDraft:     {},
		StatusSubmitted: {StatusSubmitted},
		StatusApproved:  {StatusSubmitted, StatusApproved},
		StatusRejected:  {StatusSubmitted, StatusRejected},
		StatusCancelled: {StatusCancelled},
		StatusClosed:    {StatusSubmitted, StatusApproved, StatusClosed},
	}
	r := newDraft(t)
	for _, step := range steps[status] {
		if err := actions[step].run(r); err != nil {
			t.Fatalf("build %s via %s: %v", status, step, err)
		}
	}
	return r
}

// actions maps each transition's target status to the method that attempts
// it (the key doubles as the resulting status when the edge is legal).
type action struct {
	run func(*Requisition) error
}

var actions = map[string]action{
	StatusSubmitted: {func(r *Requisition) error { return r.Submit(testNow, testActor) }},
	StatusApproved:  {func(r *Requisition) error { return r.Approve(testNow, testActor) }},
	StatusRejected:  {func(r *Requisition) error { return r.Reject(testNow, testActor) }},
	StatusCancelled: {func(r *Requisition) error { return r.Cancel(testNow, testActor) }},
	StatusClosed:    {func(r *Requisition) error { return r.Close(testNow, testActor) }},
}

var allStatuses = []string{
	StatusDraft, StatusSubmitted, StatusApproved, StatusRejected, StatusCancelled, StatusClosed,
}

func TestNewDraft(t *testing.T) {
	r := newDraft(t)
	if r.Status != StatusDraft || r.Currency != CurrencyIDR {
		t.Fatalf("status/currency = %q/%q", r.Status, r.Currency)
	}
	if r.TotalCents != 2*8500000+2*1500000 {
		t.Fatalf("total = %d, want 20000000", r.TotalCents)
	}
	if got := r.Total(); got != (Money{AmountCents: 20000000, Currency: CurrencyIDR}) {
		t.Fatalf("Total() = %+v", got)
	}
	events := r.Events()
	if len(events) != 1 || events[0].Type != EventCreated {
		t.Fatalf("events = %+v, want one %s", events, EventCreated)
	}

	// PullEvents drains: the aggregate keeps working, later pulls are empty.
	pulled := r.PullEvents()
	if len(pulled) != 1 || pulled[0].Type != EventCreated {
		t.Fatalf("pulled = %+v", pulled)
	}
	if got := r.Events(); len(got) != 0 {
		t.Fatalf("after pull, events = %+v", got)
	}
	if got := r.PullEvents(); len(got) != 0 {
		t.Fatalf("second pull = %+v", got)
	}
}

func TestNewValidation(t *testing.T) {
	base := func() NewInput {
		return NewInput{
			TenantID: "t", Number: "PR-1", RequesterID: "u",
			Currency: CurrencyIDR, Items: validItems(), Now: testNow, CreatedBy: "u",
		}
	}
	for name, tc := range map[string]struct {
		mutate func(*NewInput)
		want   error
	}{
		"currency outside allowlist": {func(in *NewInput) { in.Currency = "EUR" }, ErrCurrencyInvalid},
		"currency lowercase":         {func(in *NewInput) { in.Currency = "idr" }, ErrCurrencyInvalid},
		"currency empty":             {func(in *NewInput) { in.Currency = "" }, ErrCurrencyInvalid},
		"qty zero":                   {func(in *NewInput) { in.Items[0].Qty = 0 }, ErrQtyInvalid},
		"qty negative":               {func(in *NewInput) { in.Items[0].Qty = -1 }, ErrQtyInvalid},
		"unit price negative":        {func(in *NewInput) { in.Items[0].UnitPriceCents = -1 }, ErrUnitPriceInvalid},
		"description empty":          {func(in *NewInput) { in.Items[0].Description = "" }, ErrDescriptionInvalid},
		"description blank":          {func(in *NewInput) { in.Items[0].Description = "   " }, ErrDescriptionInvalid},
		"zero price is free not bad": {func(in *NewInput) { in.Items[0].UnitPriceCents = 0 }, nil},
		"empty items draft ok":       {func(in *NewInput) { in.Items = nil }, nil},
	} {
		in := base()
		tc.mutate(&in)
		r, err := New(in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
			continue
		}
		if tc.want == nil && r == nil {
			t.Errorf("%s: nil aggregate without error", name)
		}
		if tc.want != nil && r != nil {
			t.Errorf("%s: non-nil aggregate with error", name)
		}
	}
}

func TestCurrencyAllowlist(t *testing.T) {
	for _, c := range []string{CurrencyIDR, CurrencyUSD} {
		if !CurrencyAllowed(c) {
			t.Errorf("CurrencyAllowed(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"EUR", "", "idr", "USDT"} {
		if CurrencyAllowed(c) {
			t.Errorf("CurrencyAllowed(%q) = true, want false", c)
		}
	}
}

// The seven legal edges: each lands in the target status and appends exactly
// one event of the matching type.
func TestLegalTransitions(t *testing.T) {
	legal := [][2]string{
		{StatusDraft, StatusSubmitted},
		{StatusDraft, StatusCancelled},
		{StatusSubmitted, StatusApproved},
		{StatusSubmitted, StatusRejected},
		{StatusSubmitted, StatusCancelled},
		{StatusApproved, StatusCancelled},
		{StatusApproved, StatusClosed},
	}
	wantEvent := map[string]string{
		StatusSubmitted: EventSubmitted,
		StatusApproved:  EventApproved,
		StatusRejected:  EventRejected,
		StatusCancelled: EventCancelled,
		StatusClosed:    EventClosed,
	}
	if len(legal) != len(transitionEvents) {
		t.Fatalf("legal set (%d) does not mirror transitionEvents (%d)", len(legal), len(transitionEvents))
	}
	for _, edge := range legal {
		from, to := edge[0], edge[1]
		t.Run(from+"->"+to, func(t *testing.T) {
			r := atStatus(t, from)
			before := len(r.Events())
			if err := actions[to].run(r); err != nil {
				t.Fatalf("transition: %v", err)
			}
			if r.Status != to {
				t.Fatalf("status = %q, want %q", r.Status, to)
			}
			events := r.Events()
			if len(events) != before+1 {
				t.Fatalf("events = %d, want %d (exactly one append)", len(events), before+1)
			}
			if last := events[len(events)-1]; last.Type != wantEvent[to] || last.Actor != testActor {
				t.Fatalf("last event = %+v, want type %s actor %s", last, wantEvent[to], testActor)
			}
		})
	}
}

// Every other cell of the 6×5 state×action matrix — including self-loops and
// moves out of terminal states — fails with ErrIllegalTransition and leaves
// the aggregate bit-for-bit unchanged.
func TestIllegalTransitions(t *testing.T) {
	legal := map[[2]string]bool{
		{StatusDraft, StatusSubmitted}:     true,
		{StatusDraft, StatusCancelled}:     true,
		{StatusSubmitted, StatusApproved}:  true,
		{StatusSubmitted, StatusRejected}:  true,
		{StatusSubmitted, StatusCancelled}: true,
		{StatusApproved, StatusCancelled}:  true,
		{StatusApproved, StatusClosed}:     true,
	}
	illegal := 0
	for _, from := range allStatuses {
		for target, act := range actions {
			if legal[[2]string{from, target}] {
				continue
			}
			illegal++
			t.Run(from+"-"+target, func(t *testing.T) {
				r := atStatus(t, from)
				eventsBefore := r.Events()
				totalBefore, submittedBefore := r.TotalCents, r.SubmittedAt
				err := act.run(r)
				if !errors.Is(err, ErrIllegalTransition) || err != ErrIllegalTransition {
					t.Fatalf("err = %v, want ErrIllegalTransition", err)
				}
				if r.Status != from {
					t.Fatalf("status = %q, illegal transition must not move it", r.Status)
				}
				if got := r.Events(); len(got) != len(eventsBefore) {
					t.Fatalf("events = %d, want %d (no append on failure)", len(got), len(eventsBefore))
				}
				if r.TotalCents != totalBefore || r.SubmittedAt != submittedBefore {
					t.Fatalf("aggregate mutated: total %d→%d, submittedAt %v→%v",
						totalBefore, r.TotalCents, submittedBefore, r.SubmittedAt)
				}
			})
		}
	}
	if illegal != 23 {
		t.Fatalf("illegal cases = %d, want 23 (6 statuses × 5 actions − 7 legal)", illegal)
	}
}

// Terminal states have no outgoing edge in the machine.
func TestTerminalStatuses(t *testing.T) {
	for _, s := range allStatuses {
		want := s == StatusRejected || s == StatusCancelled || s == StatusClosed
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

// Submit re-validates the basket: an empty draft cannot enter approval, and
// an invalid line injected after creation is caught at the gate.
func TestSubmitValidation(t *testing.T) {
	r, err := New(NewInput{
		TenantID: "t", Number: "PR-1", RequesterID: "u", Currency: CurrencyUSD,
		Now: testNow, CreatedBy: "u", // no items yet
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Submit(testNow, testActor); !errors.Is(err, ErrItemsEmpty) {
		t.Fatalf("empty submit: err = %v, want ErrItemsEmpty", err)
	}
	if r.Status != StatusDraft || r.SubmittedAt != nil || len(r.Events()) != 1 {
		t.Fatalf("failed submit mutated the aggregate: %+v", r)
	}

	r.Items = append(r.Items, Item{Description: "ghost line", Qty: 0, UnitPriceCents: 100})
	if err := r.Submit(testNow, testActor); !errors.Is(err, ErrQtyInvalid) {
		t.Fatalf("invalid-item submit: err = %v, want ErrQtyInvalid", err)
	}
	if r.Status != StatusDraft || len(r.Events()) != 1 {
		t.Fatalf("failed submit mutated the aggregate: %+v", r)
	}

	if err := r.UpdateItems(validItems(), testNow, testActor); err != nil {
		t.Fatalf("UpdateItems: %v", err)
	}
	if err := r.Submit(testNow, testActor); err != nil {
		t.Fatalf("submit after fixing items: %v", err)
	}
	if r.SubmittedAt == nil || !r.SubmittedAt.Equal(testNow) {
		t.Fatalf("submittedAt = %v, want %v", r.SubmittedAt, testNow)
	}
}

func TestUpdateItems(t *testing.T) {
	r := newDraft(t)
	replacement := []Item{{Description: "Monitor 27\"", Qty: 1, UnitPriceCents: 3500000, Category: "IT"}}
	later := testNow.Add(time.Hour)
	if err := r.UpdateItems(replacement, later, "editor-1"); err != nil {
		t.Fatalf("UpdateItems: %v", err)
	}
	if len(r.Items) != 1 || r.TotalCents != 3500000 {
		t.Fatalf("items/total = %+v/%d", r.Items, r.TotalCents)
	}
	if !r.UpdatedAt.Equal(later) || r.UpdatedBy != "editor-1" {
		t.Fatalf("audit not stamped: %v %q", r.UpdatedAt, r.UpdatedBy)
	}
	if got := r.Events(); len(got) != 1 { // creation only: item edits are not transitions
		t.Fatalf("events = %+v, want creation only", got)
	}

	// Invalid replacement changes nothing.
	bad := []Item{{Description: "x", Qty: -2, UnitPriceCents: 1}}
	if err := r.UpdateItems(bad, later, "editor-1"); !errors.Is(err, ErrQtyInvalid) {
		t.Fatalf("bad items: err = %v, want ErrQtyInvalid", err)
	}
	if len(r.Items) != 1 || r.TotalCents != 3500000 {
		t.Fatalf("failed update mutated the aggregate: %+v", r)
	}

	// Non-draft requisitions are immutable.
	submitted := atStatus(t, StatusSubmitted)
	if err := submitted.UpdateItems(replacement, later, "editor-1"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("update after submit: err = %v, want ErrIllegalTransition", err)
	}
}

// Events() returns a copy: mutating it cannot rewrite history.
func TestEventsCopy(t *testing.T) {
	r := newDraft(t)
	events := r.Events()
	events[0].Type = "tampered"
	if r.Events()[0].Type != EventCreated {
		t.Fatalf("Events() leaked the internal slice")
	}
}
