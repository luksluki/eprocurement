// Package server wires the procurement gRPC server: the shared platform
// scaffolding plus the procurement.v1 API and the service-layer RBAC
// re-check with this service's method permissions.
package server

import (
	"log/slog"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/grpcserver"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
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
// receipt guards are domain checks inside the transitions. Posting a goods
// receipt is buyer/admin too (grn:post; docs/rbac.md) — the cumulative and
// status guards are domain checks inside the posting transaction. Creating
// an invoice is finance/admin (invoice:write — the money-records gate) and
// the detail read is the broad invoice:read; the over-billing, duplicate,
// and PO-status guards are domain checks inside the creation transaction.
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
	"/procurement.v1.Procurement/IssuePurchaseOrder":       rbac.POWrite,
	"/procurement.v1.Procurement/AcknowledgePurchaseOrder": rbac.POWrite,
	"/procurement.v1.Procurement/CancelPurchaseOrder":      rbac.POWrite,
	"/procurement.v1.Procurement/ClosePurchaseOrder":       rbac.POWrite,
	"/procurement.v1.Procurement/PostGoodsReceipt":         rbac.GRNPost,
	"/procurement.v1.Procurement/CreateInvoice":            rbac.InvoiceWrite,
	"/procurement.v1.Procurement/GetInvoice":               rbac.InvoiceRead,
}

// New returns a grpcserver.Server with the procurement API registered.
func New(log *slog.Logger, pinger grpcserver.Pinger, store *vendor.Store, requisitions *requisition.Store, approvals *approval.Store, purchaseOrders *purchaseorder.Store, goodsReceipts *goodsreceipt.Store, invoices *invoice.Store, opts ...grpcserver.Option) *grpcserver.Server {
	srv := grpcserver.New(log, pinger, append([]grpcserver.Option{
		grpcserver.WithServiceName(ServiceName),
		grpcserver.WithUnary(rbac.UnaryInterceptor(methodPermissions)),
	}, opts...)...)
	procurementv1.RegisterProcurementServer(srv.GRPC(), &api{
		store: store, requisitions: requisitions, approvals: approvals,
		purchaseOrders: purchaseOrders, goodsReceipts: goodsReceipts,
		invoices: invoices, log: log,
	})
	return srv
}
