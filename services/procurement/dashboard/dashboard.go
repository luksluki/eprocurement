// Package dashboard is the FR-09 read model: one tenant-scoped snapshot with
// the pending-approval queue, the per-vendor spend over trailing windows,
// and the purchase-order status distribution. Pure read side — no state, no
// events; every query runs through the platform tenant context (RLS), so
// cross-tenant rows are invisible by construction.
package dashboard

import (
	"time"

	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
)

// Widget bounds. The pending list is the dashboard's first page, not the
// whole queue (the total count tells the rest); the spend table is the top
// vendors by 90-day spend so the payload stays one screen.
const (
	// PendingPageSize bounds the approvals widget's first page.
	PendingPageSize = 5
	// SpendVendorLimit bounds the spend widget to the top vendors by 90-day
	// spend (the wider window; its 30-day column may still be zero).
	SpendVendorLimit = 10
)

// SpendWindow30d and SpendWindow90d are the trailing windows of the spend
// widget (FR-09), anchored on the caller-supplied clock (the store takes now
// as a parameter so tests pin it).
const (
	SpendWindow30d = 30 * 24 * time.Hour
	SpendWindow90d = 90 * 24 * time.Hour
)

// POStatusOrder is the canonical FR-06 lifecycle order the distribution
// widget renders — every status appears, zeros included.
var POStatusOrder = []string{
	purchaseorder.StatusDraft,
	purchaseorder.StatusIssued,
	purchaseorder.StatusAcknowledged,
	purchaseorder.StatusPartiallyReceived,
	purchaseorder.StatusReceived,
	purchaseorder.StatusClosed,
	purchaseorder.StatusCancelled,
}

// PendingApproval is one current pending step with the requisition header it
// blocks: the current step is the smallest pending step_no of a requisition
// in submitted — exactly the step the decide API accepts.
type PendingApproval struct {
	RequisitionID string
	Number        string
	Currency      string
	TotalCents    int64
	SubmittedAt   time.Time
	StepID        string
	StepNo        int32
	RoleRequired  string
}

// PendingApprovals is the approvals widget: the total number of current
// pending steps across the tenant's submitted requisitions plus the first
// page of them, oldest-submitted first.
type PendingApprovals struct {
	Total int
	Items []PendingApproval
}

// VendorSpend is one vendor+currency row of the spend widget: the sum of
// purchase_orders.total_cents over POs issued inside each trailing window
// with status neither draft nor cancelled (the #49 register basis).
type VendorSpend struct {
	VendorID      string
	VendorName    string
	Currency      string
	SpendCents30d int64
	SpendCents90d int64
}

// StatusCount is one FR-06 status with its live row count.
type StatusCount struct {
	Status string
	Count  int64
}

// CurrencySpend is one currency's unbounded window sum — the KPI cards'
// spend numbers (money never mixes currencies: a tenant may hold IDR and USD
// side by side, so the totals stay per currency).
type CurrencySpend struct {
	Currency       string
	ThirtyDayCents int64
	NinetyDayCents int64
}

// Summary is the whole dashboard snapshot served by one round trip.
type Summary struct {
	Pending              PendingApprovals
	SpendByVendor        []VendorSpend
	POStatusDistribution []StatusCount
	// SpendTotals holds one CurrencySpend per currency with issued spend in
	// the 90-day window, largest 90-day sum first.
	SpendTotals []CurrencySpend
}
