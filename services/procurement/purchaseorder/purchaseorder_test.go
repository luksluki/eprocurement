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
