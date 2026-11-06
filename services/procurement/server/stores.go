// Consumer-side store interfaces for the procurement API (#118): one small
// interface per domain, holding exactly the method surface api.go calls.
// Method signatures are copied verbatim from the concrete *Store types —
// including the pgx.Tx-carrying Submit materialize callback and the tx-scoped
// MaterializeTx — so the concrete stores satisfy them with zero changes (the
// compile-time assertions below prove it) and the API layer can be unit-tested
// with in-memory fakes instead of a PostgreSQL-backed store. The stores remain
// the integration contract: api_test.go exercises them against the real
// database through the same interfaces.
package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/dashboard"
	"github.com/luksluki/eprocurement/services/procurement/export"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/match"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

// VendorStore is the vendor surface the API uses (GetForUse/ForUseTx belong
// to the PO conversion's store, not the API).
type VendorStore interface {
	Create(ctx context.Context, tenantID string, in vendor.CreateInput) (vendor.Vendor, error)
	Get(ctx context.Context, tenantID, id string) (vendor.Vendor, error)
	Update(ctx context.Context, tenantID string, in vendor.UpdateInput) (vendor.Vendor, error)
	Deactivate(ctx context.Context, tenantID, id, userID string) (vendor.Vendor, error)
	List(ctx context.Context, tenantID string, f vendor.ListFilter) ([]vendor.Vendor, int, error)
}

// RequisitionStore is the requisition surface the API uses. Submit keeps the
// concrete store's pgx.Tx-carrying materialize callback verbatim: the handler
// passes a closure over the approval store's MaterializeTx, and the fakes
// honor the same contract with a nil tx.
type RequisitionStore interface {
	Create(ctx context.Context, tenantID string, in requisition.CreateInput) (requisition.Requisition, error)
	Get(ctx context.Context, tenantID, id string) (requisition.Requisition, error)
	UpdateItems(ctx context.Context, tenantID, id string, items []requisition.Item, actor string) (requisition.Requisition, error)
	Submit(ctx context.Context, tenantID, id, actor, idempotencyKey string, materialize func(tx pgx.Tx, r requisition.Requisition) error) (requisition.Requisition, error)
	List(ctx context.Context, tenantID string, f requisition.ListFilter) ([]requisition.Requisition, int, error)
}

// ApprovalStore is the approval surface the API uses, including the
// tx-scoped MaterializeTx the submit handler's callback invokes.
type ApprovalStore interface {
	ListRules(ctx context.Context, tenantID string) ([]approval.Rule, error)
	ReplaceRules(ctx context.Context, tenantID string, rules []approval.Rule) ([]approval.Rule, error)
	MaterializeTx(ctx context.Context, tx pgx.Tx, r requisition.Requisition) ([]approval.Step, error)
	ListSteps(ctx context.Context, tenantID, requisitionID string) ([]approval.Step, error)
	Decide(ctx context.Context, tenantID string, in approval.DecideInput) (requisition.Requisition, []approval.Step, error)
}

// PurchaseOrderStore is the purchase-order surface the API uses: the
// conversion plus the four lifecycle transitions and the list/detail reads.
type PurchaseOrderStore interface {
	CreateFromRequisition(ctx context.Context, tenantID, requisitionID, vendorID, actor string) (purchaseorder.PurchaseOrder, bool, error)
	Issue(ctx context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error)
	Acknowledge(ctx context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error)
	Cancel(ctx context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error)
	Close(ctx context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error)
	List(ctx context.Context, tenantID string, f purchaseorder.ListFilter) ([]purchaseorder.PurchaseOrder, int, error)
	Get(ctx context.Context, tenantID, id string) (purchaseorder.PurchaseOrder, error)
}

// GoodsReceiptStore is the goods-receipt surface the API uses.
type GoodsReceiptStore interface {
	Post(ctx context.Context, tenantID string, in goodsreceipt.PostInput) (goodsreceipt.GoodsReceipt, purchaseorder.PurchaseOrder, error)
}

// InvoiceStore is the invoice surface the API uses.
type InvoiceStore interface {
	Create(ctx context.Context, tenantID string, in invoice.CreateInput) (invoice.Invoice, error)
	Get(ctx context.Context, tenantID, id string) (invoice.Invoice, error)
	List(ctx context.Context, tenantID string, f invoice.ListFilter) ([]invoice.Invoice, int, error)
}

// MatchStore is the three-way-match surface the API uses.
type MatchStore interface {
	Match(ctx context.Context, tenantID, invoiceID, actor string) (invoice.Invoice, match.Result, error)
	GetSettings(ctx context.Context, tenantID string) (match.Settings, error)
	ReplaceSettings(ctx context.Context, tenantID string, in match.Settings, actor string) (match.Settings, error)
}

// DashboardStore is the dashboard read-model surface the API uses.
type DashboardStore interface {
	Summary(ctx context.Context, tenantID string, now time.Time) (dashboard.Summary, error)
}

// ExportStore is the manual-export surface the API uses.
type ExportStore interface {
	Request(ctx context.Context, tenantID, actorID, actorIP, traceID string, occurredAt time.Time) (string, error)
}

// Compile-time proofs that every concrete store satisfies its interface with
// no signature or behavior change.
var (
	_ VendorStore        = (*vendor.Store)(nil)
	_ RequisitionStore   = (*requisition.Store)(nil)
	_ ApprovalStore      = (*approval.Store)(nil)
	_ PurchaseOrderStore = (*purchaseorder.Store)(nil)
	_ GoodsReceiptStore  = (*goodsreceipt.Store)(nil)
	_ InvoiceStore       = (*invoice.Store)(nil)
	_ MatchStore         = (*match.Store)(nil)
	_ DashboardStore     = (*dashboard.Store)(nil)
	_ ExportStore        = (*export.Store)(nil)
)
