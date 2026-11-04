package goodsreceipt

import (
	"errors"
	"strings"
	"testing"
)

// validLines is the shape-valid two-line receipt the failure table mutates.
func validLines() []Item {
	return []Item{
		{POItemID: "55555555-0000-0000-0000-0000000000a1", QtyReceived: 1},
		{POItemID: "55555555-0000-0000-0000-0000000000a2", QtyReceived: 2},
	}
}

// AC (#37): the request shape is validated before any transaction opens —
// at least one line, every quantity > 0 (receipts are cumulative, so a zero
// or negative line is a client bug, not a no-op), no line twice, notes
// within the 1000-char bound. Line membership (the po_item belongs to this
// PO) is the purchase-order aggregate's guard, tested in the purchaseorder
// package.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*[]Item, *string)
		wantErr error
	}{
		{name: "happy path", wantErr: nil},
		{
			name:    "empty basket",
			mutate:  func(items *[]Item, _ *string) { *items = nil },
			wantErr: ErrItemsEmpty,
		},
		{
			name: "zero qty",
			mutate: func(items *[]Item, _ *string) {
				(*items)[0].QtyReceived = 0
			},
			wantErr: ErrQtyInvalid,
		},
		{
			name: "negative qty",
			mutate: func(items *[]Item, _ *string) {
				(*items)[1].QtyReceived = -3
			},
			wantErr: ErrQtyInvalid,
		},
		{
			name: "duplicate line",
			mutate: func(items *[]Item, _ *string) {
				(*items)[1].POItemID = (*items)[0].POItemID
			},
			wantErr: ErrPOItemDuplicate,
		},
		{
			name: "notes over bound",
			mutate: func(_ *[]Item, notes *string) {
				*notes = strings.Repeat("x", MaxNotesLen+1)
			},
			wantErr: ErrNotesInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := validLines()
			notes := "dock 3, slightly damaged pallet"
			if tc.mutate != nil {
				tc.mutate(&items, &notes)
			}
			err := Validate(items, notes)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
