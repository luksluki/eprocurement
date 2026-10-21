// Package server wires the procurement gRPC server: the shared platform
// scaffolding plus the procurement.v1 API and the service-layer RBAC
// re-check with this service's method permissions.
package server

import (
	"log/slog"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/grpcserver"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

// ServiceName is the readiness health-check service name.
const ServiceName = "procurement"

// methodPermissions is the defense-in-depth map: the gateway enforces these
// permissions too, but the service re-checks them from the propagated roles.
var methodPermissions = rbac.MethodPermissions{
	"/procurement.v1.Procurement/CreateVendor":     rbac.VendorWrite,
	"/procurement.v1.Procurement/GetVendor":        rbac.VendorRead,
	"/procurement.v1.Procurement/UpdateVendor":     rbac.VendorWrite,
	"/procurement.v1.Procurement/DeactivateVendor": rbac.VendorWrite,
}

// New returns a grpcserver.Server with the procurement API registered.
func New(log *slog.Logger, pinger grpcserver.Pinger, store *vendor.Store, opts ...grpcserver.Option) *grpcserver.Server {
	srv := grpcserver.New(log, pinger, append([]grpcserver.Option{
		grpcserver.WithServiceName(ServiceName),
		grpcserver.WithUnary(rbac.UnaryInterceptor(methodPermissions)),
	}, opts...)...)
	procurementv1.RegisterProcurementServer(srv.GRPC(), &api{store: store, log: log})
	return srv
}
