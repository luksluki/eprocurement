package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/luksluki/eprocurement/services/procurement/requisition"
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

// ListVendors through the full gRPC wiring: pagination, filters, defaults,
// validation, and cross-tenant isolation.
func TestVendorGRPCList(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()

	// Seed a small, exact set (cleaned by prefix).
	seed := func(tenant, code, name string) {
		t.Helper()
		_, err := client.CreateVendor(callerCtx(tenant, rbac.RoleBuyer),
			&procurementv1.CreateVendorRequest{Code: code, Name: name})
		if err != nil {
			t.Fatalf("create %s: %v", code, err)
		}
	}
	seed(gTenantA, "L27-A", "PT Athaya Nusantara")
	seed(gTenantA, "L27-B", "PT Bunda Sejahtera")
	seed(gTenantA, "L27-C", "PT Cempaka Nusantara")
	seed(gTenantB, "L27-A", "PT Athaya Nusantara") // other tenant, invisible
	t.Cleanup(func() {
		_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`DELETE FROM procurement.vendors WHERE code LIKE 'L27-%'`)
			return err
		})
	})

	// Page defaults with a code-prefix query (the prefix also keeps the
	// assertion independent of unrelated rows in the shared test tenants):
	// page 1, page size 20; ordered by code; other tenant's row never
	// appears.
	resp, err := client.ListVendors(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListVendorsRequest{Query: "l27-"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.GetPage() != 1 || resp.GetPageSize() != 20 || resp.GetTotal() != 3 {
		t.Fatalf("defaults: page=%d size=%d total=%d", resp.GetPage(), resp.GetPageSize(), resp.GetTotal())
	}
	if len(resp.GetVendors()) != 3 || resp.GetVendors()[0].GetCode() != "L27-A" {
		t.Fatalf("vendors = %v", resp.GetVendors())
	}

	// Combined filters: name substring + status, still tenant-scoped.
	resp, err = client.ListVendors(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.ListVendorsRequest{Query: "nusantara", Status: "active", Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("combined: %v", err)
	}
	if resp.GetTotal() != 2 || len(resp.GetVendors()) != 2 || resp.GetPageSize() != 2 {
		t.Fatalf("combined: total=%d rows=%d size=%d", resp.GetTotal(), len(resp.GetVendors()), resp.GetPageSize())
	}

	// Cross-tenant listing: tenant B sees only its own row.
	resp, err = client.ListVendors(callerCtx(gTenantB, rbac.RoleViewer),
		&procurementv1.ListVendorsRequest{Query: "l27-", PageSize: 50})
	if err != nil {
		t.Fatalf("tenantB list: %v", err)
	}
	if resp.GetTotal() != 1 || resp.GetVendors()[0].GetTenantId() != gTenantB {
		t.Fatalf("tenantB sees %+v", resp.GetVendors())
	}

	// Validation: unknown status and absurd page are InvalidArgument with
	// machine codes.
	for _, req := range []*procurementv1.ListVendorsRequest{
		{Status: "paused"},
		{Page: 2000000},
	} {
		_, err := client.ListVendors(callerCtx(gTenantA, rbac.RoleViewer), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("validation %+v: code = %v, want InvalidArgument", req, status.Code(err))
		}
	}

	// No auth metadata: Unauthenticated.
	if _, err := client.ListVendors(context.Background(),
		&procurementv1.ListVendorsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
}

// The #28 contract, as a pure unit test (no database): illegal state-machine
// transitions map to codes.Aborted — the gateway (#29) renders Aborted as
// HTTP 409 — and validation failures map to InvalidArgument with the machine
// code carried verbatim in the gRPC message.
func TestMapRequisitionError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
		msg  string
	}{
		"illegal transition":        {requisition.ErrIllegalTransition, codes.Aborted, "requisition_illegal_transition"},
		"currency invalid":          {requisition.ErrCurrencyInvalid, codes.InvalidArgument, "requisition_currency_invalid"},
		"description invalid":       {requisition.ErrDescriptionInvalid, codes.InvalidArgument, "requisition_description_invalid"},
		"qty invalid":               {requisition.ErrQtyInvalid, codes.InvalidArgument, "requisition_qty_invalid"},
		"unit price invalid":        {requisition.ErrUnitPriceInvalid, codes.InvalidArgument, "requisition_unit_price_invalid"},
		"items empty":               {requisition.ErrItemsEmpty, codes.InvalidArgument, "requisition_items_empty"},
		"unknown error is internal": {errors.New("boom"), codes.Internal, "internal error"},
	} {
		got := mapRequisitionError(tc.err)
		if status.Code(got) != tc.code || status.Convert(got).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)",
				name, status.Code(got), status.Convert(got).Message(), tc.code, tc.msg)
		}
	}

	// Wrapping the sentinel (callers add context with %w) must not change
	// the code — errors.Is still recognizes it.
	got := mapRequisitionError(fmt.Errorf("submit: %w", requisition.ErrIllegalTransition))
	if status.Code(got) != codes.Aborted {
		t.Errorf("wrapped sentinel: code = %v, want Aborted", status.Code(got))
	}
}
