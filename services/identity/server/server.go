// Package server wires the identity gRPC server: the shared platform
// scaffolding (health, trace/log/recovery interceptors) plus the identity.v1
// API and the service-layer RBAC re-check.
//
// Liveness (health service "") tracks the process; readiness (service
// "identity") tracks the PostgreSQL ping loop.
package server

import (
	"log/slog"
	"time"

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	"github.com/luksluki/eprocurement/internal/platform/grpcserver"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// ServiceName is the health-check service name whose status tracks PostgreSQL
// readiness; the empty service name ("") always reports SERVING while the
// process is up.
const ServiceName = "identity"

// Pinger reports whether the service's dependencies are reachable.
type Pinger = grpcserver.Pinger

// Option adapts identity server options to the platform server.
type Option func(*options)

type options struct {
	healthInterval time.Duration
	svc            *auth.Service
}

// WithHealthInterval overrides the readiness ping interval (tests use short values).
func WithHealthInterval(d time.Duration) Option {
	return func(o *options) { o.healthInterval = d }
}

// WithIdentityAPI registers the identity.v1 API (login/refresh/logout/jwks).
func WithIdentityAPI(svc *auth.Service) Option {
	return func(o *options) { o.svc = svc }
}

// New returns a grpcserver.Server with identity's API registered. The
// service-layer RBAC interceptor carries identity's method permissions —
// empty today: Login/Refresh/Logout/GetJwks are pre-auth or internal, and
// user-facing RPCs (ListUsers, AssignRoles, ...) register theirs when they
// arrive.
func New(log *slog.Logger, pinger Pinger, opts ...Option) *grpcserver.Server {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	srv := grpcserver.New(log, pinger,
		grpcserver.WithServiceName(ServiceName),
		grpcserver.WithHealthInterval(o.healthInterval),
		grpcserver.WithUnary(rbac.UnaryInterceptor(rbac.MethodPermissions{})),
	)
	if o.svc != nil {
		identityv1.RegisterIdentityServer(srv.GRPC(), newAPI(log, o.svc))
	}
	return srv
}
