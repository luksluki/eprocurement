package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

const (
	gTenantA = "dddddddd-0000-0000-0000-000000000001"
	gTenantB = "dddddddd-0000-0000-0000-000000000002"
	gUser    = "dddddddd-0000-0000-0000-0000000000aa"
	gUserB   = "dddddddd-0000-0000-0000-0000000000bb"
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
	srv := New(log, db, vendor.NewStore(db), requisition.NewStore(db), approval.NewStore(db))

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
	return callerCtxAs(gUser, tenantID, roles...)
}

// callerCtxAs is callerCtx with an explicit user id (ownership tests).
func callerCtxAs(userID, tenantID string, roles ...string) context.Context {
	md := metadata.Pairs(rbac.MDUserID, userID, rbac.MDTenantID, tenantID)
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
		"id invalid":                {requisition.ErrIDInvalid, codes.InvalidArgument, "requisition_id_invalid"},
		"key invalid":               {requisition.ErrIdempotencyKeyInvalid, codes.InvalidArgument, "requisition_idempotency_key_invalid"},
		"list status invalid":       {requisition.ErrStatusInvalid, codes.InvalidArgument, "requisition_status_invalid"},
		"list page invalid":         {requisition.ErrPageInvalid, codes.InvalidArgument, "requisition_page_invalid"},
		"not found":                 {requisition.ErrNotFound, codes.NotFound, "requisition_not_found"},
		"key conflict":              {requisition.ErrIdempotencyKeyConflict, codes.AlreadyExists, "requisition_idempotency_key_conflict"},
		"forbidden":                 {requisition.ErrForbidden, codes.PermissionDenied, "requisition_forbidden"},
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

// grpcCleanupRequisition wipes the PR rows, counters, and approval rules
// this test creates in the shared test tenants (items and steps cascade).
func grpcCleanupRequisition(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.outbox WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisitions WHERE tenant_id IN ($1, $2) AND number LIKE 'PR-%'`,
			gTenantA, gTenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisition_counters WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.approval_rules WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB)
		return err
	})
}

func testItems() []*procurementv1.RequisitionItem {
	return []*procurementv1.RequisitionItem{
		{Description: "Laptop", Qty: 2, UnitPriceCents: 7500000, Category: "it"},
		{Description: "Mouse", Qty: 3, UnitPriceCents: 150000, Category: "it"},
	}
}

// Full requisition lifecycle through the gRPC API: RBAC, ownership, server
// side totals/numbering, idempotent submit, and cross-tenant denial.
func TestRequisitionGRPCLifecycle(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	// A viewer cannot create (service-layer RBAC re-check).
	_, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer create: code = %v, want PermissionDenied", status.Code(err))
	}

	// Draft with no items; currency is normalized (uppercased/trimmed) at the
	// API boundary, the number is generated per tenant, the total is 0.
	empty, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: " idr "})
	if err != nil {
		t.Fatalf("create empty draft: %v", err)
	}
	if empty.GetStatus() != "draft" || empty.GetCurrency() != "IDR" || empty.GetTotalCents() != 0 {
		t.Fatalf("empty draft = %+v", empty)
	}
	if empty.GetRequesterId() != gUser || empty.GetCreatedBy() != gUser {
		t.Fatalf("requester/audit wrong: %+v", empty)
	}
	if !regexp.MustCompile(`^PR-\d{6}$`).MatchString(empty.GetNumber()) {
		t.Fatalf("number %q not PR-%%06d", empty.GetNumber())
	}

	// Draft with items: total computed server-side (2×7,500,000 + 3×150,000).
	basket, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create basket: %v", err)
	}
	if basket.GetTotalCents() != 15450000 {
		t.Fatalf("total = %d, want 15450000", basket.GetTotalCents())
	}
	numEmpty, _ := strconv.Atoi(strings.TrimPrefix(empty.GetNumber(), "PR-"))
	numBasket, _ := strconv.Atoi(strings.TrimPrefix(basket.GetNumber(), "PR-"))
	if numBasket != numEmpty+1 {
		t.Fatalf("numbers not sequential: %d then %d", numEmpty, numBasket)
	}

	// Edit replaces the basket and recomputes the total (owner).
	edited, err := client.UpdateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: testItems()[:1]})
	if err != nil || edited.GetTotalCents() != 15000000 || len(edited.GetItems()) != 1 {
		t.Fatalf("edit = (%+v, %v)", edited, err)
	}

	// Ownership (maintainer decision 2026-10-23): another requester of the
	// same tenant may not edit or submit; an admin may.
	_, err = client.UpdateRequisition(callerCtxAs(gUserB, gTenantA, rbac.RoleRequester),
		&procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: testItems()})
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "requisition_forbidden" {
		t.Fatalf("other requester edit: (%v, %q), want PermissionDenied requisition_forbidden", status.Code(err), status.Convert(err).Message())
	}
	if _, err := client.SubmitRequisition(callerCtxAs(gUserB, gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: basket.GetId()}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("other requester submit: code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := client.UpdateRequisition(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: testItems()}); err != nil {
		t.Fatalf("admin edit: %v", err)
	}

	// Cross-tenant edit/submit is indistinguishable from missing.
	for name, call := range map[string]func() error{
		"edit": func() error {
			_, err := client.UpdateRequisition(callerCtx(gTenantB, rbac.RoleRequester),
				&procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: testItems()})
			return err
		},
		"submit": func() error {
			_, err := client.SubmitRequisition(callerCtx(gTenantB, rbac.RoleRequester),
				&procurementv1.SubmitRequisitionRequest{Id: basket.GetId()})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.NotFound || status.Convert(err).Message() != "requisition_not_found" {
			t.Fatalf("cross-tenant %s: (%v, %q), want NotFound requisition_not_found", name, status.Code(err), status.Convert(err).Message())
		}
	}

	// Non-UUID id fails fast with 400 instead of a database cast error.
	_, err = client.UpdateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.UpdateRequisitionRequest{Id: "not-a-uuid"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "requisition_id_invalid" {
		t.Fatalf("bad id: (%v, %q)", status.Code(err), status.Convert(err).Message())
	}

	// Submit requires at least one line.
	_, err = client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: empty.GetId()})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "requisition_items_empty" {
		t.Fatalf("empty submit: (%v, %q), want InvalidArgument requisition_items_empty", status.Code(err), status.Convert(err).Message())
	}

	// Since #30 a submit also needs applicable approval rules — configure a
	// minimal IDR chain for this tenant before submitting (admins only).
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
		}}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}

	// Submit with an Idempotency-Key; a replay of the same key returns the
	// current (submitted) state, not an error.
	submitted, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: basket.GetId(), IdempotencyKey: "key-29-a"})
	if err != nil || submitted.GetStatus() != "submitted" || submitted.GetSubmittedAt() == "" {
		t.Fatalf("submit = (%+v, %v)", submitted, err)
	}
	// The submit response carries the materialized steps in-band (#30).
	if len(submitted.GetApprovalSteps()) != 1 || submitted.GetApprovalSteps()[0].GetId() == "" {
		t.Fatalf("submit steps = %+v, want one step with an id", submitted.GetApprovalSteps())
	}
	replayed, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: basket.GetId(), IdempotencyKey: "key-29-a"})
	if err != nil || replayed.GetStatus() != "submitted" || replayed.GetId() != basket.GetId() {
		t.Fatalf("replay = (%+v, %v)", replayed, err)
	}

	// Reusing the key for a different requisition of the tenant conflicts.
	third, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create third: %v", err)
	}
	_, err = client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: third.GetId(), IdempotencyKey: "key-29-a"})
	if status.Code(err) != codes.AlreadyExists || status.Convert(err).Message() != "requisition_idempotency_key_conflict" {
		t.Fatalf("key reuse: (%v, %q), want AlreadyExists requisition_idempotency_key_conflict", status.Code(err), status.Convert(err).Message())
	}

	// A submitted requisition is immutable: edit and resubmit are 409s.
	_, err = client.UpdateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.UpdateRequisitionRequest{Id: basket.GetId(), Items: testItems()})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "requisition_illegal_transition" {
		t.Fatalf("edit submitted: (%v, %q), want Aborted requisition_illegal_transition", status.Code(err), status.Convert(err).Message())
	}
	if _, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: basket.GetId(), IdempotencyKey: "key-29-b"}); status.Code(err) != codes.Aborted {
		t.Fatalf("resubmit: code = %v, want Aborted", status.Code(err))
	}

	// Over-long idempotency key is rejected at the service layer too.
	if _, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: third.GetId(), IdempotencyKey: strings.Repeat("k", 65)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("long key: code = %v, want InvalidArgument", status.Code(err))
	}
}

// Validation failures carry the domain machine code verbatim in the gRPC
// message (same contract as the vendor API).
func TestRequisitionGRPCValidation(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	valid := testItems()
	for name, tc := range map[string]struct {
		req  *procurementv1.CreateRequisitionRequest
		code codes.Code
		msg  string
	}{
		"no currency":    {&procurementv1.CreateRequisitionRequest{Items: valid}, codes.InvalidArgument, "requisition_currency_invalid"},
		"bad currency":   {&procurementv1.CreateRequisitionRequest{Currency: "EUR", Items: valid}, codes.InvalidArgument, "requisition_currency_invalid"},
		"bad qty":        {&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: []*procurementv1.RequisitionItem{{Description: "x", Qty: 0}}}, codes.InvalidArgument, "requisition_qty_invalid"},
		"negative price": {&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: []*procurementv1.RequisitionItem{{Description: "x", Qty: 1, UnitPriceCents: -1}}}, codes.InvalidArgument, "requisition_unit_price_invalid"},
		"blank desc":     {&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: []*procurementv1.RequisitionItem{{Description: "  ", Qty: 1}}}, codes.InvalidArgument, "requisition_description_invalid"},
	} {
		_, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester), tc.req)
		if status.Code(err) != tc.code || status.Convert(err).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", name, status.Code(err), status.Convert(err).Message(), tc.code, tc.msg)
		}
	}

	// No propagated auth metadata at all: Unauthenticated before any store hit.
	if _, err := client.CreateRequisition(context.Background(),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
}

// ListRequisitions through the full gRPC wiring: defaults, number-prefix and
// status filters, header-only rows, newest-first ordering, cross-tenant
// invisibility, validation codes, and the Unauthenticated gate.
func TestRequisitionGRPCList(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	// Three drafts in tenant A plus one submitted (the filter shapes need a
	// non-draft status); one row in tenant B that must never surface.
	create := func(tenant string) *procurementv1.Requisition {
		r, err := client.CreateRequisition(callerCtx(tenant, rbac.RoleRequester),
			&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
		if err != nil {
			t.Fatalf("create %s: %v", tenant, err)
		}
		return r
	}
	a1, _, a3 := create(gTenantA), create(gTenantA), create(gTenantA)
	submitted := create(gTenantA)
	create(gTenantB)
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	if _, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: submitted.GetId()}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Defaults: page 1, page size 20, tenant A's four rows newest first.
	resp, err := client.ListRequisitions(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListRequisitionsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.GetPage() != 1 || resp.GetPageSize() != 20 || resp.GetTotal() != 4 || len(resp.GetRequisitions()) != 4 {
		t.Fatalf("defaults: page=%d size=%d total=%d rows=%d", resp.GetPage(), resp.GetPageSize(), resp.GetTotal(), len(resp.GetRequisitions()))
	}
	rows := resp.GetRequisitions()
	if rows[0].GetNumber() != submitted.GetNumber() || rows[3].GetNumber() != a1.GetNumber() {
		t.Fatalf("not newest-first: %s…%s", rows[0].GetNumber(), rows[3].GetNumber())
	}
	// Header-only: items and approval_steps are never populated on list rows.
	for _, r := range rows {
		if len(r.GetItems()) != 0 || len(r.GetApprovalSteps()) != 0 {
			t.Fatalf("list row carries a body: %+v", r)
		}
		if r.GetId() == "" || r.GetTenantId() != gTenantA || r.GetCreatedAt() == "" {
			t.Fatalf("header fields missing: %+v", r)
		}
	}

	// Status filter: only the submitted row.
	resp, err = client.ListRequisitions(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListRequisitionsRequest{Status: "submitted", PageSize: 10})
	if err != nil || resp.GetTotal() != 1 || resp.GetRequisitions()[0].GetId() != submitted.GetId() {
		t.Fatalf("status filter = (%+v, %v)", resp, err)
	}

	// Number-prefix query, case-insensitive input: matches a3 only.
	resp, err = client.ListRequisitions(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListRequisitionsRequest{Query: strings.ToLower(a3.GetNumber())})
	if err != nil || resp.GetTotal() != 1 || resp.GetRequisitions()[0].GetId() != a3.GetId() {
		t.Fatalf("prefix query = (%+v, %v)", resp, err)
	}

	// Combined filters: status among a prefix set (a2 and a3 share the tenant
	// counter sequence; use the common "PR-" prefix + draft → the three drafts
	// minus none, ordered newest first).
	resp, err = client.ListRequisitions(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListRequisitionsRequest{Query: "PR-", Status: "draft", Page: 1, PageSize: 2})
	if err != nil || resp.GetTotal() != 3 || len(resp.GetRequisitions()) != 2 ||
		resp.GetRequisitions()[0].GetId() != a3.GetId() {
		t.Fatalf("combined = (%+v, %v)", resp, err)
	}

	// Past-the-end page: empty rows, true total via the COUNT fallback.
	resp, err = client.ListRequisitions(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListRequisitionsRequest{Page: 9, PageSize: 2})
	if err != nil || len(resp.GetRequisitions()) != 0 || resp.GetTotal() != 4 {
		t.Fatalf("past-the-end = (%+v, %v)", resp, err)
	}

	// Cross-tenant listing: tenant B sees only its own row.
	resp, err = client.ListRequisitions(callerCtx(gTenantB, rbac.RoleViewer),
		&procurementv1.ListRequisitionsRequest{})
	if err != nil || resp.GetTotal() != 1 || resp.GetRequisitions()[0].GetTenantId() != gTenantB {
		t.Fatalf("tenantB sees (%+v, %v)", resp, err)
	}

	// Validation: unknown status and absurd page are InvalidArgument.
	for _, req := range []*procurementv1.ListRequisitionsRequest{
		{Status: "paused"},
		{Page: 2000000},
	} {
		_, err := client.ListRequisitions(callerCtx(gTenantA, rbac.RoleViewer), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("validation %+v: code = %v, want InvalidArgument", req, status.Code(err))
		}
	}

	// No auth metadata: Unauthenticated.
	if _, err := client.ListRequisitions(context.Background(),
		&procurementv1.ListRequisitionsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
}

// testRules is the two-step IDR chain (approver@5M then finance@10M) plus a
// USD rule that must never match an IDR requisition.
func testRules() []*procurementv1.ApprovalRule {
	return []*procurementv1.ApprovalRule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 5_000_000},
		{Currency: "IDR", StepNo: 2, RoleRequired: "finance", ThresholdCents: 10_000_000},
		{Currency: "USD", StepNo: 1, RoleRequired: "approver", ThresholdCents: 1},
	}
}

// Approval-rule administration: admin-only at the interceptor, replace-set
// with edge normalization, validation machine codes, and the GET round-trip.
func TestApprovalRulesGRPC(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	// approval:configure is admin-only: requester and viewer are denied at
	// the service-layer re-check.
	for _, role := range []string{rbac.RoleRequester, rbac.RoleViewer, rbac.RoleApprover} {
		if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, role),
			&procurementv1.ReplaceApprovalRulesRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("replace as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
		if _, err := client.GetApprovalRules(callerCtx(gTenantA, role),
			&procurementv1.GetApprovalRulesRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("get as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
	}

	// Empty tenant: no rules.
	got, err := client.GetApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.GetApprovalRulesRequest{})
	if err != nil || len(got.GetRules()) != 0 {
		t.Fatalf("initial get = (%v, %v), want empty", got, err)
	}

	// Replace with edge-case normalization: currency uppercased, role
	// lowercased; ids are server-generated.
	stored, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: " idr ", StepNo: 2, RoleRequired: " Finance ", ThresholdCents: 10_000_000},
			{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 5_000_000},
		}})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if len(stored.GetRules()) != 2 ||
		stored.GetRules()[0].GetCurrency() != "IDR" || stored.GetRules()[0].GetRoleRequired() != "approver" ||
		stored.GetRules()[1].GetRoleRequired() != "finance" || stored.GetRules()[1].GetId() == "" {
		t.Fatalf("stored rules = %+v", stored.GetRules())
	}
	if got, err := client.GetApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.GetApprovalRulesRequest{}); err != nil || len(got.GetRules()) != 2 {
		t.Fatalf("get after replace = (%v, %v), want 2 rules", got, err)
	}

	// Validation failures carry the machine codes verbatim.
	for name, tc := range map[string]struct {
		rule *procurementv1.ApprovalRule
		msg  string
	}{
		"bad currency":       {&procurementv1.ApprovalRule{Currency: "EUR", StepNo: 1, RoleRequired: "approver"}, "approval_rule_currency_invalid"},
		"bad role":           {&procurementv1.ApprovalRule{Currency: "IDR", StepNo: 1, RoleRequired: "cto"}, "approval_rule_role_invalid"},
		"negative threshold": {&procurementv1.ApprovalRule{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: -1}, "approval_rule_threshold_invalid"},
		"zero step_no":       {&procurementv1.ApprovalRule{Currency: "IDR", StepNo: 0, RoleRequired: "approver"}, "approval_rule_step_no_invalid"},
	} {
		_, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
			&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{tc.rule}})
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (InvalidArgument, %q)", name, status.Code(err), status.Convert(err).Message(), tc.msg)
		}
	}
	// Duplicate step_no per currency.
	_, err = client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "IDR", StepNo: 1, RoleRequired: "approver"},
			{Currency: "IDR", StepNo: 1, RoleRequired: "finance"},
		}})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "approval_rule_step_no_duplicate" {
		t.Fatalf("dup step_no: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	// The failed replaces changed nothing.
	if got, err := client.GetApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.GetApprovalRulesRequest{}); err != nil || len(got.GetRules()) != 2 {
		t.Fatalf("get after invalid replaces = (%v, %v), want the previous 2", got, err)
	}

	// Tenant B's rules are invisible to tenant A (and vice versa): each
	// tenant configures independently.
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantB, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: []*procurementv1.ApprovalRule{
			{Currency: "USD", StepNo: 1, RoleRequired: "buyer", ThresholdCents: 0},
		}}); err != nil {
		t.Fatalf("tenant B replace: %v", err)
	}
	gotA, err := client.GetApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin), &procurementv1.GetApprovalRulesRequest{})
	if err != nil || len(gotA.GetRules()) != 2 || gotA.GetRules()[0].GetCurrency() != "IDR" {
		t.Fatalf("tenant A sees (%v, %v) after tenant B configured", gotA.GetRules(), err)
	}
}

// The #30 approval flow through the gRPC API: submit without rules is a 412
// that rolls back (the key is not recorded), submit materializes steps
// in-band, sequential decisions with role/self enforcement, and the
// reject-skips flow.
func TestApprovalGRPCFlow(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	// No rules yet: submit fails with FailedPrecondition (HTTP 412 via the
	// gateway) and the requisition stays a draft.
	pr, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: pr.GetId(), IdempotencyKey: "key-30-a"})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "approval_rules_missing" {
		t.Fatalf("submit without rules: got (%v, %q), want FailedPrecondition approval_rules_missing", status.Code(err), status.Convert(err).Message())
	}
	if still, err := client.UpdateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.UpdateRequisitionRequest{Id: pr.GetId(), Items: testItems()}); err != nil || still.GetStatus() != "draft" {
		t.Fatalf("after failed submit = (%s, %v), want still draft", still.GetStatus(), err)
	}

	// Configure rules; the SAME key now names the fresh submit — proof the
	// 412 did not record it.
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	submitted, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: pr.GetId(), IdempotencyKey: "key-30-a"})
	if err != nil || submitted.GetStatus() != "submitted" {
		t.Fatalf("submit after rules = (%+v, %v)", submitted, err)
	}
	// AC: the response carries the materialized steps in-band — both IDR
	// thresholds match (15,450,000 >= 5M and 10M), the USD rule does not.
	steps := submitted.GetApprovalSteps()
	if len(steps) != 2 || steps[0].GetStepNo() != 1 || steps[1].GetStepNo() != 2 ||
		steps[0].GetId() == "" || steps[1].GetId() == "" || steps[0].GetStatus() != "pending" {
		t.Fatalf("submit steps = %+v, want two pending steps with ids", steps)
	}
	// Replay of the key: current state, no second transition, no duplicate
	// steps.
	if replayed, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: pr.GetId(), IdempotencyKey: "key-30-a"}); err != nil ||
		replayed.GetStatus() != "submitted" || len(replayed.GetApprovalSteps()) != 2 ||
		replayed.GetSubmittedAt() != submitted.GetSubmittedAt() {
		t.Fatalf("replay = (%+v, %v)", replayed, err)
	}

	// A viewer holds no approval:decide — the interceptor denies.
	if _, err := client.DecideApproval(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "approve"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer decide: code = %v, want PermissionDenied", status.Code(err))
	}

	// Self-approval is universally forbidden: the requester holds approver
	// (and even admin) — still approval_self_forbidden.
	for _, roles := range [][]string{{rbac.RoleApprover}, {rbac.RoleAdmin}, {rbac.RoleRequester, rbac.RoleApprover}} {
		_, err := client.DecideApproval(callerCtxAs(gUser, gTenantA, roles...),
			&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "approve"})
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "approval_self_forbidden" {
			t.Fatalf("self decide %v: got (%v, %q), want PermissionDenied approval_self_forbidden",
				roles, status.Code(err), status.Convert(err).Message())
		}
	}

	// Out-of-order: the finance step is not current while step 1 pends (the
	// decider holds finance AND approver — approval:decide is approver-only
	// in the matrix, so a finance-step decider is dual-roled by definition).
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleFinance, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[1].GetId(), Decision: "approve"})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "approval_step_not_current" {
		t.Fatalf("out-of-order decide: got (%v, %q), want Aborted approval_step_not_current", status.Code(err), status.Convert(err).Message())
	}

	// Role enforcement: a pure approver cannot decide the finance step —
	// approval_role_forbidden.
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[1].GetId(), Decision: "approve"})
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "approval_role_forbidden" {
		t.Fatalf("approver on finance step: got (%v, %q), want PermissionDenied approval_role_forbidden", status.Code(err), status.Convert(err).Message())
	}

	// Decision validation: unknown value, and reject without reason.
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "maybe"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "approval_decision_invalid" {
		t.Fatalf("bad decision: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "reject", Reason: "   "})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "approval_reason_required" {
		t.Fatalf("reject without reason: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}

	// Approve step 1 as a pure approver; the response embeds the updated
	// steps and the requisition stays submitted mid-flow.
	after1, err := client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "approve"})
	if err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	if r := after1.GetRequisition(); r.GetStatus() != "submitted" || len(r.GetApprovalSteps()) != 2 ||
		r.GetApprovalSteps()[0].GetStatus() != "approved" || r.GetApprovalSteps()[1].GetStatus() != "pending" {
		t.Fatalf("after step 1 = %+v", r)
	}
	if r := after1.GetRequisition().GetApprovalSteps()[0]; r.GetDecidedBy() != gUserB || r.GetDecidedAt() == "" {
		t.Fatalf("step 1 decision fields = %+v", r)
	}

	// Cross-tenant decide: tenant B cannot see the step (404, distinct
	// from forbidden) — the caller holds approval:decide, so only the RLS
	// invisibility can answer.
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantB, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[1].GetId(), Decision: "approve"})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "approval_step_not_found" {
		t.Fatalf("cross-tenant decide: got (%v, %q), want NotFound approval_step_not_found", status.Code(err), status.Convert(err).Message())
	}

	// A dual-role finance+approver user approves the last step → the
	// requisition is approved.
	after2, err := client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleFinance, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[1].GetId(), Decision: "approve", Reason: "within budget"})
	if err != nil {
		t.Fatalf("approve step 2: %v", err)
	}
	if r := after2.GetRequisition(); r.GetStatus() != "approved" ||
		r.GetApprovalSteps()[1].GetStatus() != "approved" || r.GetApprovalSteps()[1].GetReason() != "within budget" {
		t.Fatalf("after step 2 = %+v", r)
	}

	// Terminal: further decisions are 409s.
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleFinance, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[1].GetId(), Decision: "approve"})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "approval_step_not_current" {
		t.Fatalf("decide after terminal: got (%v, %q), want Aborted approval_step_not_current", status.Code(err), status.Convert(err).Message())
	}

	// Bad step id fails fast; unknown step id reads as missing.
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: "not-a-uuid", Decision: "approve"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "approval_step_id_invalid" {
		t.Fatalf("bad step id: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: "dddddddd-0000-0000-0000-0000000000ff", Decision: "approve"})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "approval_step_not_found" {
		t.Fatalf("unknown step id: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
}

// Reject stops the flow: the step is rejected, downstream pending steps are
// skipped, the requisition is rejected, and every later decision is a 409.
// An admin may decide someone else's step, but never their own.
func TestApprovalGRPCRejectFlow(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	pr, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	submitted, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: pr.GetId(), IdempotencyKey: "key-30-reject"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	steps := submitted.GetApprovalSteps()

	// An admin rejects step 1 — a different user than the requester, or
	// the universal self-approval ban would fire first.
	after, err := client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleAdmin),
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "reject", Reason: "over budget"})
	if err != nil {
		t.Fatalf("reject step 1: %v", err)
	}
	r := after.GetRequisition()
	if r.GetStatus() != "rejected" ||
		r.GetApprovalSteps()[0].GetStatus() != "rejected" ||
		r.GetApprovalSteps()[1].GetStatus() != "skipped" {
		t.Fatalf("after reject = %s %+v", r.GetStatus(), r.GetApprovalSteps())
	}
	if s := r.GetApprovalSteps()[0]; s.GetReason() != "over budget" || s.GetDecidedBy() != gUserB {
		t.Fatalf("rejected step fields = %+v", s)
	}
	if s := r.GetApprovalSteps()[1]; s.GetDecidedBy() != "" || s.GetDecidedAt() != "" {
		t.Fatalf("skipped step records a decision: %+v", s)
	}

	// Every further decision on the dead flow is a 409 — including by the
	// admin on the skipped step.
	for _, s := range r.GetApprovalSteps() {
		_, err := client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleAdmin),
			&procurementv1.DecideApprovalRequest{StepId: s.GetId(), Decision: "approve"})
		if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "approval_step_not_current" {
			t.Fatalf("decide %s after reject: got (%v, %q), want Aborted approval_step_not_current", s.GetId(), status.Code(err), status.Convert(err).Message())
		}
	}
}

// Decide idempotency + outbox over the real gRPC surface: the service-layer
// key bound, the replay contract (current state, no second event), the
// cross-step key conflict, and outbox rows carrying the caller as actor and
// the propagated x-trace-id.
func TestApprovalGRPCDecideIdempotencyOutbox(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	pr, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	submitted, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: pr.GetId()})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	steps := submitted.GetApprovalSteps()

	outboxCount := func() int {
		var n int
		if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`SELECT count(*) FROM procurement.outbox WHERE entity_id = $1`, pr.GetId()).Scan(&n)
		}); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		return n
	}

	// Service-layer key bound (the gateway re-checks the same limit at the
	// edge); nothing was written.
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "approve",
			IdempotencyKey: strings.Repeat("k", 65)})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "approval_idempotency_key_invalid" {
		t.Fatalf("long key: got (%v, %q), want InvalidArgument approval_idempotency_key_invalid", status.Code(err), status.Convert(err).Message())
	}
	if n := outboxCount(); n != 0 {
		t.Fatalf("outbox after rejected key = %d, want 0", n)
	}

	// A keyed decide whose gRPC call carries x-trace-id: the outbox row must
	// carry the same trace id and the caller as actor (PRD §18).
	decider := metadata.AppendToOutgoingContext(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover), "x-trace-id", "trace-grpc-31")
	after1, err := client.DecideApproval(decider,
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "approve",
			IdempotencyKey: "grpc-decide-31"})
	if err != nil || after1.GetRequisition().GetStatus() != "submitted" {
		t.Fatalf("keyed decide = (%+v, %v)", after1, err)
	}
	if n := outboxCount(); n != 1 {
		t.Fatalf("outbox after mid-flow decide = %d, want 1", n)
	}
	var eventType, actorID, traceID string
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT event_type, actor_id::text, trace_id
			FROM procurement.outbox WHERE entity_id = $1`, pr.GetId()).Scan(&eventType, &actorID, &traceID)
	}); err != nil {
		t.Fatalf("load outbox row: %v", err)
	}
	if eventType != "pr.step_approved" || actorID != gUserB || traceID != "trace-grpc-31" {
		t.Fatalf("outbox row = (%s, %s, %s), want pr.step_approved by %s with trace-grpc-31", eventType, actorID, traceID, gUserB)
	}

	// Replay: the same key on the same step returns the identical state.
	replay, err := client.DecideApproval(decider,
		&procurementv1.DecideApprovalRequest{StepId: steps[0].GetId(), Decision: "approve",
			IdempotencyKey: "grpc-decide-31"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	a, b := replay.GetRequisition().GetApprovalSteps()[0], after1.GetRequisition().GetApprovalSteps()[0]
	if a.GetDecidedAt() != b.GetDecidedAt() || a.GetDecidedBy() != b.GetDecidedBy() {
		t.Fatalf("replay step = %+v, want %+v", a, b)
	}
	if n := outboxCount(); n != 1 {
		t.Fatalf("outbox after replay = %d, want 1", n)
	}

	// The same key cannot name another step of the tenant.
	_, err = client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleFinance, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[1].GetId(), Decision: "approve",
			IdempotencyKey: "grpc-decide-31"})
	if status.Code(err) != codes.AlreadyExists || status.Convert(err).Message() != "approval_idempotency_key_conflict" {
		t.Fatalf("key on other step: got (%v, %q), want AlreadyExists approval_idempotency_key_conflict", status.Code(err), status.Convert(err).Message())
	}

	// Terminal decide under a fresh key adds the terminal event.
	after2, err := client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleFinance, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps[1].GetId(), Decision: "approve",
			IdempotencyKey: "grpc-decide-31-b"})
	if err != nil || after2.GetRequisition().GetStatus() != "approved" {
		t.Fatalf("terminal decide = (%s, %v)", after2.GetRequisition().GetStatus(), err)
	}
	if n := outboxCount(); n != 3 {
		t.Fatalf("outbox after terminal decide = %d, want 3", n)
	}
}
