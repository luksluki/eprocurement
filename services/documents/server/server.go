// Package server wires the documents gRPC server: the shared platform
// scaffolding plus the documents.v1 API and the service-layer RBAC
// re-check with this service's method permissions.
package server

import (
	"log/slog"
	"time"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/grpcserver"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/documents/attachment"
	"github.com/luksluki/eprocurement/services/documents/export"
	"github.com/luksluki/eprocurement/services/documents/history"
)

// ServiceName is the readiness health-check service name.
const ServiceName = "documents"

// methodPermissions is the defense-in-depth map: the gateway enforces these
// permissions too, but the service re-checks them from the propagated
// roles. ListEvents maps to requisition:read — the events history's
// entities today are requisitions, and requisition:read is held by every
// tenant role (rbac matrix), so the whole tenant can read its timeline
// while a method missing from this map would pass through unprotected
// (see rbac.UnaryInterceptor). The export methods map to export:run
// (#49) — finance/admin only, the permission's first real consumer.
//
// The attachment methods are deliberately NOT in this map: their permission
// depends on the attachment's entity type (vendor:read/write vs
// requisition:read/write), which the interceptor cannot see. Each handler
// re-checks the per-entity permission itself (entityPermissions in api.go)
// right after the entity type is known — the same defense-in-depth depth,
// one level deeper.
var methodPermissions = rbac.MethodPermissions{
	"/documents.v1.Documents/ListEvents":           rbac.RequisitionRead,
	"/documents.v1.Documents/ListExportRuns":       rbac.ExportRun,
	"/documents.v1.Documents/GetExportDownloadUrl": rbac.ExportRun,
}

// New returns a grpcserver.Server with the documents API registered.
func New(log *slog.Logger, pinger grpcserver.Pinger, store *attachment.Store,
	storage attachment.Storage, limits attachment.Limits, events *history.Store,
	exports *export.Store, opts ...grpcserver.Option) *grpcserver.Server {
	srv := grpcserver.New(log, pinger, append([]grpcserver.Option{
		grpcserver.WithServiceName(ServiceName),
		grpcserver.WithUnary(rbac.UnaryInterceptor(methodPermissions)),
	}, opts...)...)
	documentsv1.RegisterDocumentsServer(srv.GRPC(), &api{
		store: store, storage: storage, limits: limits, now: time.Now, log: log,
		events: events, exports: exports,
	})
	return srv
}
