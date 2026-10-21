package server

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

const (
	gTenantA = "dddddddd-0000-0000-0000-000000000001"
	gTenantB = "dddddddd-0000-0000-0000-000000000002"
	gUser    = "dddddddd-0000-0000-0000-0000000000aa"
)

// startProcurement boots the full gRPC wiring (platform server + RBAC map +
// vendor API) against a real database.
func startProcurement(t *testing.T) (procurementv1.ProcurementClient, *pgxpool.Pool, func()) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	migCfg, _ := pgxpool.ParseConfig(dsn)
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		t.Fatalf("open migration pool: %v", err)
	}
	if _, err := dbmigrate.Apply(t.Context(), migPool, "procurement", migrations.FS()); err != nil {
		migPool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	migPool.Close()

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	srv := New(log, db, vendor.NewStore(db))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(context.Background(), lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return procurementv1.NewProcurementClient(conn), db, func() {
		_ = conn.Close()
		_ = lis.Close()
		db.Close()
	}
}

// callerCtx mimics the gateway: user/tenant/roles as outgoing metadata.
func callerCtx(tenantID string, roles ...string) context.Context {
	md := metadata.Pairs(rbac.MDUserID, gUser, rbac.MDTenantID, tenantID)
	for _, r := range roles {
		md.Append(rbac.MDRole, r)
	}
	return metadata.NewOutgoingContext(context.Background(), md)
}

func grpcCleanupVendor(t *testing.T, db *pgxpool.Pool, code string) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code = $3`, gTenantA, gTenantB, code)
		return err
	})
}

// Full lifecycle through the gRPC API, with the RBAC map and the propagated
// auth context exercised for real.
func TestVendorGRPCLifecycle(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupVendor(t, db, "G-01")
	t.Cleanup(func() { grpcCleanupVendor(t, db, "G-01") })

	// A viewer cannot create (service-layer RBAC re-check).
	_, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.CreateVendorRequest{Code: "G-01", Name: "PT Denied"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer create: code = %v, want PermissionDenied", status.Code(err))
	}

	// A buyer can.
	v, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "g-01", Name: "PT Contoh", ContactEmail: "c@example.com"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.GetCode() != "G-01" || v.GetStatus() != "active" || v.GetTenantId() != gTenantA {
		t.Fatalf("vendor = %+v", v)
	}
	// AC: audit fields in the response.
	if v.GetCreatedAt() == "" || v.GetCreatedBy() != gUser || v.GetUpdatedBy() != gUser {
		t.Fatalf("audit fields missing: %+v", v)
	}

	// AC: duplicate code rejected — same tenant, different caller role.
	_, err = client.CreateVendor(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.CreateVendorRequest{Code: "G-01", Name: "PT Impostor"})
	if status.Code(err) != codes.AlreadyExists || status.Convert(err).Message() != "vendor_duplicate_code" {
		t.Fatalf("duplicate: (%v, %q), want AlreadyExists vendor_duplicate_code", status.Code(err), status.Convert(err).Message())
	}

	// Read from another tenant: not found.
	if _, err := client.GetVendor(callerCtx(gTenantB, rbac.RoleViewer),
		&procurementv1.GetVendorRequest{Id: v.GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant get: code = %v, want NotFound", status.Code(err))
	}

	// Update as buyer.
	updated, err := client.UpdateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.UpdateVendorRequest{Id: v.GetId(), Name: "PT Contoh Jaya"})
	if err != nil || updated.GetName() != "PT Contoh Jaya" {
		t.Fatalf("update = (%+v, %v)", updated, err)
	}

	// Deactivate, then the deactivated vendor is unusable and immutable.
	deactivated, err := client.DeactivateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.DeactivateVendorRequest{Id: v.GetId()})
	if err != nil || deactivated.GetStatus() != "deactivated" {
		t.Fatalf("deactivate = (%+v, %v)", deactivated, err)
	}
	if _, err := client.UpdateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.UpdateVendorRequest{Id: v.GetId(), Name: "PT Zombie"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("update after deactivate: code = %v, want FailedPrecondition", status.Code(err))
	}
}

// Validation failures carry the machine code in the gRPC message; a call
// without caller metadata is rejected before touching the store.
func TestVendorGRPCValidation(t *testing.T) {
	client, _, cleanup := startProcurement(t)
	defer cleanup()

	for name, tc := range map[string]struct {
		req  *procurementv1.CreateVendorRequest
		code codes.Code
		msg  string
	}{
		"bad code":  {&procurementv1.CreateVendorRequest{Code: "!!", Name: "PT X"}, codes.InvalidArgument, "vendor_code_invalid"},
		"bad name":  {&procurementv1.CreateVendorRequest{Code: "V-1", Name: ""}, codes.InvalidArgument, "vendor_name_invalid"},
		"bad email": {&procurementv1.CreateVendorRequest{Code: "V-1", Name: "PT X", ContactEmail: "nope"}, codes.InvalidArgument, "vendor_email_invalid"},
	} {
		_, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer), tc.req)
		if status.Code(err) != tc.code || status.Convert(err).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", name, status.Code(err), status.Convert(err).Message(), tc.code, tc.msg)
		}
	}

	// No propagated auth metadata at all: Unauthenticated before any store hit.
	_, err := client.CreateVendor(context.Background(),
		&procurementv1.CreateVendorRequest{Code: "V-9", Name: "PT Ghost"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no metadata: code = %v, want Unauthenticated", status.Code(err))
	}
}
