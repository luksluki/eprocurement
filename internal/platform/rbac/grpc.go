package rbac

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// gRPC metadata keys the gateway injects with the verified token's claims
// (ADR-0001 tenant/role propagation; the gateway side lands with #23).
const (
	MDUserID   = "x-user-id"
	MDTenantID = "x-tenant-id"
	MDRole     = "x-role" // repeated
)

// MethodPermissions maps a full gRPC method ("/identity.v1.Identity/ListUsers")
// to the permission it requires. Methods absent from the map are internal or
// pre-auth and pass through — map every user-facing method.
type MethodPermissions map[string]string

// UnaryInterceptor enforces the method permission map against the roles in
// the incoming metadata. It is the service-layer re-check behind the gateway
// (defense in depth): the gateway has already verified the JWT, but a service
// must not trust the edge alone.
func UnaryInterceptor(perms MethodPermissions) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		permission, protected := perms[info.FullMethod]
		if !protected {
			return handler(ctx, req)
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "auth context missing: protected method called without caller metadata")
		}
		roles := normalizedRoles(md.Get(MDRole))
		if len(roles) == 0 {
			return nil, status.Error(codes.Unauthenticated, "auth context missing: no caller roles in metadata")
		}
		if !Check(roles, permission) {
			return nil, status.Errorf(codes.PermissionDenied,
				"permission_denied: requires %s", permission)
		}
		return handler(ctx, req)
	}
}

func normalizedRoles(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// AuthContext extracts the propagated caller identity from gRPC metadata.
// Services use it to scope queries (tenant) and audit (user).
type AuthContext struct {
	UserID   string
	TenantID string
	Roles    []string
}

// FromMetadata returns the caller identity, or nil when the call carries none
// (e.g. service-to-service or pre-auth calls).
func FromMetadata(ctx context.Context) *AuthContext {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	roles := normalizedRoles(md.Get(MDRole))
	user := first(md.Get(MDUserID))
	tenant := first(md.Get(MDTenantID))
	if user == "" && tenant == "" && len(roles) == 0 {
		return nil
	}
	return &AuthContext{UserID: user, TenantID: tenant, Roles: roles}
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
