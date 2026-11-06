// Package server wires the procurement gRPC server: the shared platform
// scaffolding plus the procurement.v1 API and the service-layer RBAC
// re-check with this service's method permissions.
package server

import (
	"log/slog"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/grpcserver"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
)

// ServiceName is the readiness health-check service name.
const ServiceName = "procurement"

// methodPermissions is the defense-in-depth map: the gateway enforces these
// permissions too, but the service re-checks them from the propagated roles.
// Requisition writes are requester/admin and the list/detail reads are the
// broad requisition:read; the requester-ownership rule on edit/submit is a
// domain check inside the API methods (docs/rbac.md). GetRequisition must
// stay in this map: a method absent from it passes through the interceptor
// without any RBAC re-check (internal/platform/rbac/grpc.go).
// Deciding an approval step is approver/admin; the step's role_required and
// the self-approval ban are enforced in the approval domain. Configuring
// rules is admin-only (approval:configure has no non-admin role).
// Converting a requisition to a PO and every PO lifecycle action
// (issue/acknowledge/cancel/close) are buyer/admin; the source-status and
// receipt guards are domain checks inside the transitions. The PO read pair
// (#38 list/detail) is the broad po:read — every role, tenant-wide. Posting
// a goods receipt is buyer/admin too (grn:post; docs/rbac.md) — the
// cumulative and status guards are domain checks inside the posting
// transaction. Creating
// an invoice is finance/admin (invoice:write — the money-records gate) and
// the reads (#39 detail, #41 list) are the broad invoice:read; the
// over-billing, duplicate, and PO-status guards are domain checks inside the
// creation transaction.
// Matching an invoice (#40) is finance/admin as well — invoice:write, the
// same money-records gate as creating one (the finance reviewer persona,
// PRD §5; no new permission for the match action). Configuring match
// tolerances is admin-only (match:configure has no non-admin role — the
// approval:configure twin). The dashboard summary (#52) is the broad
// dashboard:read — every role, tenant-wide; it only aggregates reads those
// roles already hold (FR-09's landing page). Triggering a manual export
// (#53) is admin-only (export:trigger has no non-admin role — the
// dlq:manage twin; finance's export:run read of the run history stays).
var methodPermissions = rbac.MethodPermissions{
	"/procurement.v1.Procurement/CreateVendor":             rbac.VendorWrite,
	"/procurement.v1.Procurement/GetVendor":                rbac.VendorRead,
	"/procurement.v1.Procurement/UpdateVendor":             rbac.VendorWrite,
	"/procurement.v1.Procurement/DeactivateVendor":         rbac.VendorWrite,
	"/procurement.v1.Procurement/ListVendors":              rbac.VendorRead,
	"/procurement.v1.Procurement/CreateRequisition":        rbac.RequisitionWrite,
	"/procurement.v1.Procurement/UpdateRequisition":        rbac.RequisitionWrite,
	"/procurement.v1.Procurement/SubmitRequisition":        rbac.RequisitionWrite,
	"/procurement.v1.Procurement/ListRequisitions":         rbac.RequisitionRead,
	"/procurement.v1.Procurement/GetRequisition":           rbac.RequisitionRead,
	"/procurement.v1.Procurement/DecideApproval":           rbac.ApprovalDecide,
	"/procurement.v1.Procurement/GetApprovalRules":         rbac.ApprovalConfigure,
	"/procurement.v1.Procurement/ReplaceApprovalRules":     rbac.ApprovalConfigure,
	"/procurement.v1.Procurement/CreatePurchaseOrder":      rbac.POWrite,
	"/procurement.v1.Procurement/ListPurchaseOrders":       rbac.PORead,
	"/procurement.v1.Procurement/GetPurchaseOrder":         rbac.PORead,
	"/procurement.v1.Procurement/IssuePurchaseOrder":       rbac.POWrite,
	"/procurement.v1.Procurement/AcknowledgePurchaseOrder": rbac.POWrite,
	"/procurement.v1.Procurement/CancelPurchaseOrder":      rbac.POWrite,
	"/procurement.v1.Procurement/ClosePurchaseOrder":       rbac.POWrite,
	"/procurement.v1.Procurement/PostGoodsReceipt":         rbac.GRNPost,
	"/procurement.v1.Procurement/CreateInvoice":            rbac.InvoiceWrite,
	"/procurement.v1.Procurement/GetInvoice":               rbac.InvoiceRead,
	"/procurement.v1.Procurement/ListInvoices":             rbac.InvoiceRead,
	"/procurement.v1.Procurement/MatchInvoice":             rbac.InvoiceWrite,
	"/procurement.v1.Procurement/GetMatchSettings":         rbac.MatchConfigure,
	"/procurement.v1.Procurement/ReplaceMatchSettings":     rbac.MatchConfigure,
	"/procurement.v1.Procurement/GetDashboard":             rbac.DashboardRead,
	"/procurement.v1.Procurement/TriggerExport":            rbac.ExportTrigger,
}

// New returns a grpcserver.Server with the procurement API registered. The
// per-domain store parameters are the consumer-side interfaces defined in
// stores.go (#118); the concrete stores satisfy them unchanged (see the
// compile-time assertions there), and in-memory fakes stand in for the API
// layer's unit tests.
func New(log *slog.Logger, pinger grpcserver.Pinger, store VendorStore, requisitions RequisitionStore, approvals ApprovalStore, purchaseOrders PurchaseOrderStore, goodsReceipts GoodsReceiptStore, invoices InvoiceStore, matches MatchStore, dashboards DashboardStore, exports ExportStore, opts ...grpcserver.Option) *grpcserver.Server {
	srv := grpcserver.New(log, pinger, append([]grpcserver.Option{
		grpcserver.WithServiceName(ServiceName),
		grpcserver.WithUnary(rbac.UnaryInterceptor(methodPermissions)),
	}, opts...)...)
	procurementv1.RegisterProcurementServer(srv.GRPC(), &api{
		store: store, requisitions: requisitions, approvals: approvals,
		purchaseOrders: purchaseOrders, goodsReceipts: goodsReceipts,
		invoices: invoices, matches: matches, dashboards: dashboards,
		exports: exports, log: log,
	})
	return srv
}
