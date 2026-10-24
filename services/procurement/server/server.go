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
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

// ServiceName is the readiness health-check service name.
const ServiceName = "procurement"

// methodPermissions is the defense-in-depth map: the gateway enforces these
// permissions too, but the service re-checks them from the propagated roles.
// Requisition writes are requester/admin and the list is the broad
// requisition:read; the requester-ownership rule on edit/submit is a domain
// check inside the API methods (docs/rbac.md).
// Deciding an approval step is approver/admin; the step's role_required and
// the self-approval ban are enforced in the approval domain. Configuring
// rules is admin-only (approval:configure has no non-admin role).
var methodPermissions = rbac.MethodPermissions{
	"/procurement.v1.Procurement/CreateVendor":         rbac.VendorWrite,
	"/procurement.v1.Procurement/GetVendor":            rbac.VendorRead,
	"/procurement.v1.Procurement/UpdateVendor":         rbac.VendorWrite,
	"/procurement.v1.Procurement/DeactivateVendor":     rbac.VendorWrite,
	"/procurement.v1.Procurement/ListVendors":          rbac.VendorRead,
	"/procurement.v1.Procurement/CreateRequisition":    rbac.RequisitionWrite,
	"/procurement.v1.Procurement/UpdateRequisition":    rbac.RequisitionWrite,
	"/procurement.v1.Procurement/SubmitRequisition":    rbac.RequisitionWrite,
	"/procurement.v1.Procurement/ListRequisitions":     rbac.RequisitionRead,
	"/procurement.v1.Procurement/DecideApproval":       rbac.ApprovalDecide,
	"/procurement.v1.Procurement/GetApprovalRules":     rbac.ApprovalConfigure,
	"/procurement.v1.Procurement/ReplaceApprovalRules": rbac.ApprovalConfigure,
}

// New returns a grpcserver.Server with the procurement API registered.
func New(log *slog.Logger, pinger grpcserver.Pinger, store *vendor.Store, requisitions *requisition.Store, approvals *approval.Store, opts ...grpcserver.Option) *grpcserver.Server {
	srv := grpcserver.New(log, pinger, append([]grpcserver.Option{
		grpcserver.WithServiceName(ServiceName),
		grpcserver.WithUnary(rbac.UnaryInterceptor(methodPermissions)),
	}, opts...)...)
	procurementv1.RegisterProcurementServer(srv.GRPC(), &api{store: store, requisitions: requisitions, approvals: approvals, log: log})
	return srv
}
