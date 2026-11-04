package rbac

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const method = "/procurement.v1.Procurement/CreateVendor"

var perms = MethodPermissions{
	method: VendorWrite,
}

func run(ctx context.Context) error {
	_, err := UnaryInterceptor(perms)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { return "ok", nil })
	return err
}

// roleCtx builds an incoming context carrying one caller with the given roles.
func roleCtx(roles ...string) context.Context {
	md := metadata.Pairs(MDUserID, "u1", MDTenantID, "t1")
	for _, r := range roles {
		md.Append(MDRole, r)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

// AC: service layer re-checks permissions — the interceptor denies roles the
// gateway would also deny, and allows those it would allow.
func TestInterceptorEnforcesMatrix(t *testing.T) {
	for _, role := range allRoles {
		wantCode := codes.OK
		if !Check([]string{role}, VendorWrite) {
			wantCode = codes.PermissionDenied
		}
		err := run(roleCtx(role))
		if status.Code(err) != wantCode {
			t.Errorf("role %s: code = %v (%v), want %v", role, status.Code(err), err, wantCode)
		}
	}
}

func TestInterceptorDenials(t *testing.T) {
	// No metadata at all on a protected method.
	if err := run(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no metadata: code = %v, want Unauthenticated", status.Code(err))
	}

	// Metadata present but no roles.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(MDUserID, "u1"))
	if err := run(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no roles: code = %v, want Unauthenticated", status.Code(err))
	}

	// Blank roles are ignored, not treated as a grant.
	md := metadata.Pairs(MDUserID, "u1")
	md.Append(MDRole, "  ")
	md.Append(MDRole, "")
	ctx = metadata.NewIncomingContext(context.Background(), md)
	if err := run(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("blank roles: code = %v, want Unauthenticated", status.Code(err))
	}

	// Insufficient roles carry the permission-denied reason with the required permission.
	denied := metadata.NewIncomingContext(context.Background(), metadata.Pairs(MDRole, RoleViewer))
	err := run(denied)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer on vendor:write: code = %v, want PermissionDenied", status.Code(err))
	}
	if want := "permission_denied: requires vendor:write"; status.Convert(err).Message() != want {
		t.Fatalf("message = %q, want %q", status.Convert(err).Message(), want)
	}
}

func TestInterceptorPassesUnmappedMethods(t *testing.T) {
	// Unmapped methods (internal/pre-auth) must not require any metadata.
	_, err := UnaryInterceptor(perms)(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/identity.v1.Identity/Login"},
		func(context.Context, any) (any, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("unmapped method should pass without metadata: %v", err)
	}
}

func TestFromMetadata(t *testing.T) {
	if FromMetadata(context.Background()) != nil {
		t.Fatal("no metadata must return nil")
	}

	md := metadata.Pairs(MDUserID, " u1 ", MDTenantID, "t1")
	md.Append(MDRole, RoleBuyer)
	md.Append(MDRole, RoleViewer)

	authCtx := FromMetadata(metadata.NewIncomingContext(context.Background(), md))
	if authCtx == nil || authCtx.UserID != "u1" || authCtx.TenantID != "t1" {
		t.Fatalf("auth context = %+v", authCtx)
	}
	if len(authCtx.Roles) != 2 || authCtx.Roles[0] != RoleBuyer || authCtx.Roles[1] != RoleViewer {
		t.Fatalf("roles = %v", authCtx.Roles)
	}
}
