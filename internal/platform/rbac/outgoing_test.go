package rbac

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

// The gateway side of the propagation: OutgoingContext must produce exactly
// what FromMetadata (the service side) reads back.
func TestOutgoingContextRoundTrip(t *testing.T) {
	ctx := OutgoingContext(context.Background(), "user-1", "tenant-1",
		[]string{RoleAdmin, " ", RoleBuyer})

	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("no outgoing metadata on context")
	}
	if got := md.Get(MDUserID); len(got) != 1 || got[0] != "user-1" {
		t.Fatalf("user id = %v", got)
	}
	if got := md.Get(MDTenantID); len(got) != 1 || got[0] != "tenant-1" {
		t.Fatalf("tenant id = %v", got)
	}
	if got := md.Get(MDRole); len(got) != 2 || got[0] != RoleAdmin || got[1] != RoleBuyer {
		t.Fatalf("roles = %v, want blank entries dropped", got)
	}

	// Round trip through the incoming side the interceptor uses.
	authCtx := FromMetadata(metadata.NewIncomingContext(context.Background(), md))
	if authCtx == nil || authCtx.UserID != "user-1" || authCtx.TenantID != "tenant-1" {
		t.Fatalf("auth context = %+v", authCtx)
	}
	if len(authCtx.Roles) != 2 {
		t.Fatalf("roles = %v", authCtx.Roles)
	}
}
