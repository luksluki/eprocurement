package server

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/luksluki/eprocurement/services/procurement/dashboard"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/match"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
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
	srv := New(log, db, vendor.NewStore(db), requisition.NewStore(db), approval.NewStore(db), purchaseorder.NewStore(db), goodsreceipt.NewStore(db), invoice.NewStore(db), match.NewStore(db), dashboard.NewStore(db))

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

// grpcCleanupRequisition wipes the PR rows, counters, approval rules, and
// PO rows this test family creates in the shared test tenants (items and
// steps cascade). Invoices go first: they hold no-cascade FKs to
// purchase_orders (#39), so PO deletion would fail underneath them.
func grpcCleanupRequisition(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.outbox WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.invoices WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.goods_receipts WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.grn_counters WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.purchase_orders WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.po_counters WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
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

// GetRequisition through the full gRPC wiring (#33): the detail read owns the
// bodies the list never carries (items, approval steps), read is tenant-wide
// (another same-tenant requester may read), cross-tenant is NotFound, and the
// method is inside the RBAC map — a metadata-less call is rejected by the
// interceptor, proving the re-check runs (methods absent from the map would
// pass through; see internal/platform/rbac/grpc.go).
func TestRequisitionGRPCGet(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	t.Cleanup(func() { grpcCleanupRequisition(t, db) })

	draft, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Another same-tenant requester reads the draft (requisition:read is
	// tenant-wide; ownership governs edit/submit only).
	got, err := client.GetRequisition(callerCtxAs(gUserB, gTenantA, rbac.RoleRequester),
		&procurementv1.GetRequisitionRequest{Id: draft.GetId()})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetId() != draft.GetId() || got.GetNumber() != draft.GetNumber() ||
		got.GetCurrency() != "IDR" || got.GetStatus() != "draft" ||
		len(got.GetItems()) != len(testItems()) || got.GetTotalCents() != 15450000 {
		t.Fatalf("detail = %+v", got)
	}
	// Items ride in storage order (created_at ties break on the random uuid,
	// so the order of one insert is not deterministic) — assert the set.
	byDesc := map[string]*procurementv1.RequisitionItem{}
	for _, it := range got.GetItems() {
		byDesc[it.GetDescription()] = it
	}
	if len(byDesc) != 2 || byDesc["Laptop"] == nil || byDesc["Mouse"] == nil ||
		byDesc["Laptop"].GetQty() != 2 || byDesc["Laptop"].GetUnitPriceCents() != 7500000 {
		t.Fatalf("items not loaded: %+v", got.GetItems())
	}
	// A draft carries no materialized steps (nil on the wire).
	if len(got.GetApprovalSteps()) != 0 {
		t.Fatalf("draft steps = %+v", got.GetApprovalSteps())
	}

	// A submitted requisition reads back with its steps (the detail read owns
	// them; list rows never do).
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	submitted, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: draft.GetId()})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	got, err = client.GetRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.GetRequisitionRequest{Id: draft.GetId()})
	if err != nil || got.GetStatus() != "submitted" {
		t.Fatalf("get submitted = (%+v, %v)", got, err)
	}
	if len(got.GetApprovalSteps()) != len(submitted.GetApprovalSteps()) ||
		got.GetApprovalSteps()[0].GetId() != submitted.GetApprovalSteps()[0].GetId() {
		t.Fatalf("steps not loaded: %+v", got.GetApprovalSteps())
	}

	// Cross-tenant read is indistinguishable from missing.
	if _, err := client.GetRequisition(callerCtx(gTenantB, rbac.RoleViewer),
		&procurementv1.GetRequisitionRequest{Id: draft.GetId()}); status.Code(err) != codes.NotFound ||
		status.Convert(err).Message() != "requisition_not_found" {
		t.Fatalf("cross-tenant get: (%v, %q), want NotFound requisition_not_found", status.Code(err), status.Convert(err).Message())
	}

	// Non-UUID id fails fast; metadata-less call is rejected by the
	// interceptor's re-check (the method must stay in methodPermissions).
	if _, err := client.GetRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.GetRequisitionRequest{Id: "not-a-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := client.GetRequisition(context.Background(),
		&procurementv1.GetRequisitionRequest{Id: draft.GetId()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
	// Tenant metadata but no roles: the interceptor's map entry fires before
	// the handler (a pass-through method would fail later in authTenant).
	noRoles := metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs(rbac.MDTenantID, gTenantA, rbac.MDUserID, gUser))
	_, err = client.GetRequisition(noRoles, &procurementv1.GetRequisitionRequest{Id: draft.GetId()})
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(status.Convert(err).Message(), "no caller roles") {
		t.Fatalf("no roles: (%v, %q), want the interceptor's Unauthenticated", status.Code(err), status.Convert(err).Message())
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

	// Decide-event counting: the requisition also carries its pr.submitted
	// row since #45, so the count is filtered to the decide event types.
	outboxCount := func() int {
		var n int
		if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`SELECT count(*) FROM procurement.outbox
				 WHERE entity_id = $1
				   AND event_type IN ('pr.step_approved', 'pr.step_rejected', 'pr.approved', 'pr.rejected')`,
				pr.GetId()).Scan(&n)
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
			FROM procurement.outbox
			WHERE entity_id = $1 AND event_type = 'pr.step_approved'`,
			pr.GetId()).Scan(&eventType, &actorID, &traceID)
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

// grpcCleanupPOVendor deletes the PO test vendors by prefix (G-PO35-%) —
// the shared vendor cleanup matches one exact code, which would leave the
// -A/-B/-D rows behind between runs of the PO suite.
func grpcCleanupPOVendor(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'G-PO35-%'`,
			gTenantA, gTenantB)
		return err
	})
}

// grpcApprovedRequisition drives the real flow to an approved requisition of
// tenant A: create (requester), submit (rules materialize), approve step 1
// (a different approver — the self-approval ban).
func grpcApprovedRequisition(t *testing.T, client procurementv1.ProcurementClient) *procurementv1.Requisition {
	t.Helper()
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
	after, err := client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{
			StepId: submitted.GetApprovalSteps()[0].GetId(), Decision: "approve",
		})
	if err != nil || after.GetRequisition().GetStatus() != "approved" {
		t.Fatalf("decide = (%s, %v), want approved", after.GetRequisition().GetStatus(), err)
	}
	return after.GetRequisition()
}

// The #35 conversion through the full gRPC wiring: RBAC (po:write is
// buyer-only), validation and gate machine codes, the draft PO with copied
// lines, the closed requisition, the single po.created outbox row carrying
// the caller as actor and the propagated trace, and the replay contract
// (different vendor_id ignored).
func TestPurchaseOrderGRPCConversion(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	grpcCleanupPOVendor(t, db)
	t.Cleanup(func() {
		grpcCleanupRequisition(t, db)
		grpcCleanupPOVendor(t, db)
	})

	// Configure the one-step IDR chain and get an approved requisition.
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	approved := grpcApprovedRequisition(t, client)

	vendorA, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-PO35-A", Name: "PT Satu"})
	if err != nil {
		t.Fatalf("create vendor A: %v", err)
	}
	vendorB, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-PO35-B", Name: "PT Dua"})
	if err != nil {
		t.Fatalf("create vendor B: %v", err)
	}

	// po:write is buyer-only: requester, approver, and viewer are denied at
	// the service-layer re-check.
	for _, role := range []string{rbac.RoleRequester, rbac.RoleViewer, rbac.RoleApprover} {
		_, err := client.CreatePurchaseOrder(callerCtx(gTenantA, role),
			&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorA.GetId()})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("convert as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
	}

	// Validation and gates with machine codes: bad vendor id, unknown
	// requisition, non-approved requisition, deactivated vendor.
	_, err = client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: "nope"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "po_vendor_id_invalid" {
		t.Fatalf("bad vendor id: got (%v, %q), want InvalidArgument po_vendor_id_invalid", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{
			RequisitionId: "dddddddd-0000-0000-0000-0000000000ff", VendorId: vendorA.GetId()})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "requisition_not_found" {
		t.Fatalf("unknown requisition: got (%v, %q), want NotFound requisition_not_found", status.Code(err), status.Convert(err).Message())
	}
	draft, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	_, err = client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: draft.GetId(), VendorId: vendorA.GetId()})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "po_requisition_not_approved" {
		t.Fatalf("draft convert: got (%v, %q), want Aborted po_requisition_not_approved", status.Code(err), status.Convert(err).Message())
	}
	dead, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-PO35-D", Name: "PT Mati"})
	if err != nil {
		t.Fatalf("create vendor D: %v", err)
	}
	if _, err := client.DeactivateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.DeactivateVendorRequest{Id: dead.GetId()}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	_, err = client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: dead.GetId()})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "vendor_deactivated" {
		t.Fatalf("deactivated vendor: got (%v, %q), want FailedPrecondition vendor_deactivated", status.Code(err), status.Convert(err).Message())
	}
	// The blocked attempts burned no number and left the requisition
	// approved — the first successful conversion below is PO-000001.
	reqStatus := func(id string) string {
		var s string
		if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`SELECT status FROM procurement.requisitions WHERE id = $1`, id).Scan(&s)
		}); err != nil {
			t.Fatalf("read status: %v", err)
		}
		return s
	}
	if got := reqStatus(approved.GetId()); got != "approved" {
		t.Fatalf("requisition after blocked convert = %s, want approved", got)
	}

	// Happy path with a trace: draft PO, lines copied, server-side total.
	buyer := metadata.AppendToOutgoingContext(callerCtx(gTenantA, rbac.RoleBuyer), "x-trace-id", "trace-grpc-35")
	wrapped, err := client.CreatePurchaseOrder(buyer,
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorA.GetId()})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !wrapped.GetCreated() {
		t.Fatal("first conversion reports created=false")
	}
	po := wrapped.GetPurchaseOrder()
	if po.GetNumber() != "PO-000001" || po.GetStatus() != "draft" || po.GetIssuedAt() != "" {
		t.Fatalf("po = %s/%s/%q, want PO-000001/draft/empty issued_at", po.GetNumber(), po.GetStatus(), po.GetIssuedAt())
	}
	if po.GetRequisitionId() != approved.GetId() || po.GetVendorId() != vendorA.GetId() || po.GetTenantId() != gTenantA {
		t.Fatalf("po linkage = %+v", po)
	}
	// Items are compared as a multiset: po_items rows inside one conversion
	// share created_at, so their load order is not the requisition's line
	// order (same note as the store test).
	if po.GetCurrency() != "IDR" || po.GetTotalCents() != 15450000 || len(po.GetItems()) != 2 ||
		po.GetCreatedBy() != gUser {
		t.Fatalf("po content = %+v", po)
	}
	poLines := map[string]bool{}
	for _, it := range po.GetItems() {
		if it.GetQtyReceived() != 0 {
			t.Fatalf("qty_received = %d on creation, want 0: %+v", it.GetQtyReceived(), po.GetItems())
		}
		poLines[fmt.Sprintf("%s/%d/%d", it.GetDescription(), it.GetQtyOrdered(), it.GetUnitPriceCents())] = true
	}
	if !poLines["Laptop/2/7500000"] || !poLines["Mouse/3/150000"] || len(poLines) != 2 {
		t.Fatalf("po lines = %v, want the copied basket", poLines)
	}
	if got := reqStatus(approved.GetId()); got != "closed" {
		t.Fatalf("requisition after convert = %s, want closed", got)
	}

	// Exactly one po.created outbox row: entity = the PO, actor = caller,
	// trace propagated, payload pins requisition_number.
	var eventType, entityType, actorID, traceID string
	var payload []byte
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT event_type, entity_type, actor_id::text, trace_id, payload
			FROM procurement.outbox WHERE entity_id = $1 AND event_type = 'po.created'`,
			po.GetId()).Scan(&eventType, &entityType, &actorID, &traceID, &payload)
	}); err != nil {
		t.Fatalf("load po.created row: %v", err)
	}
	if eventType != "po.created" || entityType != "purchase_order" || actorID != gUser || traceID != "trace-grpc-35" {
		t.Fatalf("po.created row = (%s, %s, %s, %s)", eventType, entityType, actorID, traceID)
	}
	var created struct {
		Number            string `json:"number"`
		RequisitionID     string `json:"requisition_id"`
		RequisitionNumber string `json:"requisition_number"`
		VendorID          string `json:"vendor_id"`
		TotalCents        int64  `json:"total_cents"`
		Currency          string `json:"currency"`
		CreatedBy         string `json:"created_by"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if created.Number != "PO-000001" || created.RequisitionID != approved.GetId() ||
		created.RequisitionNumber != approved.GetNumber() || created.VendorID != vendorA.GetId() ||
		created.TotalCents != 15450000 || created.Currency != "IDR" || created.CreatedBy != gUser {
		t.Fatalf("po.created payload = %+v", created)
	}

	// Replay with a different vendor_id: the original PO (created=false),
	// no second event.
	replayResp, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorB.GetId()})
	if err != nil || replayResp.GetCreated() {
		t.Fatalf("replay flag = (%v, %v), want created=false", replayResp.GetCreated(), err)
	}
	replayed := replayResp.GetPurchaseOrder()
	if replayed.GetId() != po.GetId() || replayed.GetVendorId() != vendorA.GetId() ||
		replayed.GetCreatedAt() != po.GetCreatedAt() {
		t.Fatalf("replay = (%+v, %v), want the original PO", replayed, err)
	}
	var poCreatedRows int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT count(*) FROM procurement.outbox WHERE entity_id = $1 AND event_type = 'po.created'`,
			po.GetId()).Scan(&poCreatedRows)
	}); err != nil || poCreatedRows != 1 {
		t.Fatalf("po.created rows after replay = (%d, %v), want 1", poCreatedRows, err)
	}

	// Cross-tenant conversion: tenant B's buyer cannot even see the
	// requisition (RLS) — indistinguishable from missing.
	_, err = client.CreatePurchaseOrder(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorA.GetId()})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "requisition_not_found" {
		t.Fatalf("cross-tenant convert: got (%v, %q), want NotFound requisition_not_found", status.Code(err), status.Convert(err).Message())
	}

	// No propagated auth metadata: Unauthenticated before any store hit.
	if _, err := client.CreatePurchaseOrder(context.Background(),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorA.GetId()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
}

// grpcForcePO flips a PO to a post-acknowledgement status and optionally
// bumps one line's qty_received — the #37 stand-in for the receipt-driven
// states the lifecycle tests need (goods receipts own those transitions).
func grpcForcePO(t *testing.T, db *pgxpool.Pool, id, status string, receipts bool) {
	t.Helper()
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx,
			`UPDATE procurement.purchase_orders SET status = $2 WHERE id = $1`, id, status); err != nil {
			return err
		}
		if receipts {
			_, err := tx.Exec(ctx, `
				UPDATE procurement.po_items SET qty_received = 1
				WHERE po_id = $1 AND id = (
					SELECT id FROM procurement.po_items WHERE po_id = $1
					ORDER BY created_at, id LIMIT 1)`, id)
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("force po %s: %v", status, err)
	}
}

// grpcPOEventCount counts one lifecycle event type's outbox rows for a PO.
func grpcPOEventCount(t *testing.T, db *pgxpool.Pool, poID, eventType string) int {
	t.Helper()
	var n int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT count(*) FROM procurement.outbox WHERE entity_id = $1 AND event_type = $2`,
			poID, eventType).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s rows: %v", eventType, err)
	}
	return n
}

// newApprovedID drives the requester flow to one more approved requisition
// (each PO conversion consumes one).
func newApprovedID(t *testing.T, client procurementv1.ProcurementClient) string {
	t.Helper()
	return grpcApprovedRequisition(t, client).GetId()
}

// The #36 lifecycle through the full gRPC wiring: RBAC (po:write is
// buyer-only on every action), the issue → acknowledged → closed happy path
// with issued_at stamped and one outbox row per transition carrying actor +
// trace, cancel (draft and after a receipt), the machine codes of every
// guard, cross-tenant invisibility, and the anonymous rejection.
func TestPurchaseOrderGRPCLifecycle(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	grpcCleanupPOVendor(t, db)
	t.Cleanup(func() {
		grpcCleanupRequisition(t, db)
		grpcCleanupPOVendor(t, db)
	})

	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	approved := grpcApprovedRequisition(t, client)
	vendorA, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-PO35-A", Name: "PT Satu"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	wrapped, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorA.GetId()})
	if err != nil || !wrapped.GetCreated() {
		t.Fatalf("convert = (%v, %v)", wrapped.GetCreated(), err)
	}
	po := wrapped.GetPurchaseOrder()

	// po:write is buyer-only on the lifecycle actions too: requester and
	// viewer are denied at the service-layer re-check.
	for _, role := range []string{rbac.RoleRequester, rbac.RoleViewer} {
		_, err := client.IssuePurchaseOrder(callerCtx(gTenantA, role),
			&procurementv1.IssuePurchaseOrderRequest{Id: po.GetId()})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("issue as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
	}

	// Machine codes: bad id, unknown PO, and an illegal edge (acknowledging
	// a draft) — all before any state moves.
	_, err = client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: "nope"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "po_id_invalid" {
		t.Fatalf("bad id: got (%v, %q), want InvalidArgument po_id_invalid", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: "dddddddd-0000-0000-0000-0000000000ff"})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "po_not_found" {
		t.Fatalf("unknown po: got (%v, %q), want NotFound po_not_found", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.AcknowledgePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: po.GetId()})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "po_illegal_transition" {
		t.Fatalf("acknowledge draft: got (%v, %q), want Aborted po_illegal_transition", status.Code(err), status.Convert(err).Message())
	}
	if got := grpcPOEventCount(t, db, po.GetId(), "po.acknowledged"); got != 0 {
		t.Fatalf("po.acknowledged rows after illegal call = %d, want 0", got)
	}

	// Happy path with a trace: issue stamps issued_at and writes po.issued.
	buyer := metadata.AppendToOutgoingContext(callerCtx(gTenantA, rbac.RoleBuyer), "x-trace-id", "trace-grpc-36")
	issued, err := client.IssuePurchaseOrder(buyer, &procurementv1.IssuePurchaseOrderRequest{Id: po.GetId()})
	if err != nil || issued.GetStatus() != "issued" {
		t.Fatalf("issue = (%s, %v), want issued", issued.GetStatus(), err)
	}
	if issued.GetIssuedAt() == "" {
		t.Fatalf("issued_at = %q, want stamped", issued.GetIssuedAt())
	}
	var eventType, actorID, traceID string
	var payload []byte
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT event_type, actor_id::text, trace_id, payload
			FROM procurement.outbox WHERE entity_id = $1 AND event_type = 'po.issued'`,
			po.GetId()).Scan(&eventType, &actorID, &traceID, &payload)
	}); err != nil {
		t.Fatalf("load po.issued row: %v", err)
	}
	if eventType != "po.issued" || actorID != gUser || traceID != "trace-grpc-36" {
		t.Fatalf("po.issued row = (%s, %s, %s)", eventType, actorID, traceID)
	}
	var issuedPayload struct {
		Status      string `json:"status"`
		PerformedBy string `json:"performed_by"`
		Number      string `json:"number"`
	}
	if err := json.Unmarshal(payload, &issuedPayload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if issuedPayload.Status != "issued" || issuedPayload.PerformedBy != gUser || issuedPayload.Number != po.GetNumber() {
		t.Fatalf("po.issued payload = %+v", issuedPayload)
	}

	// A second issue loses the edge: 409, no second event.
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: po.GetId()}); status.Code(err) != codes.Aborted {
		t.Fatalf("double issue: code = %v, want Aborted", status.Code(err))
	}
	if got := grpcPOEventCount(t, db, po.GetId(), "po.issued"); got != 1 {
		t.Fatalf("po.issued rows after double issue = %d, want 1", got)
	}

	// Acknowledge, then the receipt-driven close (short-close arm).
	acknowledged, err := client.AcknowledgePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: po.GetId()})
	if err != nil || acknowledged.GetStatus() != "acknowledged" {
		t.Fatalf("acknowledge = (%s, %v)", acknowledged.GetStatus(), err)
	}
	if got := grpcPOEventCount(t, db, po.GetId(), "po.acknowledged"); got != 1 {
		t.Fatalf("po.acknowledged rows = %d, want 1", got)
	}
	grpcForcePO(t, db, po.GetId(), "partially_received", true)
	closed, err := client.ClosePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.ClosePurchaseOrderRequest{Id: po.GetId()})
	if err != nil || closed.GetStatus() != "closed" {
		t.Fatalf("close = (%s, %v), want closed", closed.GetStatus(), err)
	}
	if got := grpcPOEventCount(t, db, po.GetId(), "po.closed"); got != 1 {
		t.Fatalf("po.closed rows = %d, want 1", got)
	}
	// Terminal: nothing leaves closed.
	if _, err := client.CancelPurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CancelPurchaseOrderRequest{Id: po.GetId()}); status.Code(err) != codes.Aborted {
		t.Fatalf("cancel closed: code = %v, want Aborted", status.Code(err))
	}

	// Cancel rules respect receipts: a PO with a receipt cannot cancel —
	// po_has_receipts, no state move, no event.
	pinnedWrap, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: newApprovedID(t, client), VendorId: vendorA.GetId()})
	if err != nil {
		t.Fatalf("convert pinned: %v", err)
	}
	pinned := pinnedWrap.GetPurchaseOrder()
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: pinned.GetId()}); err != nil {
		t.Fatalf("issue pinned: %v", err)
	}
	if _, err := client.AcknowledgePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: pinned.GetId()}); err != nil {
		t.Fatalf("acknowledge pinned: %v", err)
	}
	grpcForcePO(t, db, pinned.GetId(), "acknowledged", true)
	_, err = client.CancelPurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CancelPurchaseOrderRequest{Id: pinned.GetId()})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "po_has_receipts" {
		t.Fatalf("cancel with receipts: got (%v, %q), want Aborted po_has_receipts", status.Code(err), status.Convert(err).Message())
	}
	var pinnedStatus string
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM procurement.purchase_orders WHERE id = $1`, pinned.GetId()).Scan(&pinnedStatus)
	}); err != nil {
		t.Fatalf("read pinned status: %v", err)
	}
	if pinnedStatus != "acknowledged" {
		t.Fatalf("pinned status = %s, want acknowledged (no move)", pinnedStatus)
	}
	if got := grpcPOEventCount(t, db, pinned.GetId(), "po.cancelled"); got != 0 {
		t.Fatalf("po.cancelled rows after pinned cancel = %d, want 0", got)
	}

	// Plain cancel from draft: po.cancelled commits.
	cancelWrap, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: newApprovedID(t, client), VendorId: vendorA.GetId()})
	if err != nil {
		t.Fatalf("convert cancel: %v", err)
	}
	cancelled, err := client.CancelPurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CancelPurchaseOrderRequest{Id: cancelWrap.GetPurchaseOrder().GetId()})
	if err != nil || cancelled.GetStatus() != "cancelled" {
		t.Fatalf("cancel = (%s, %v), want cancelled", cancelled.GetStatus(), err)
	}
	if got := grpcPOEventCount(t, db, cancelWrap.GetPurchaseOrder().GetId(), "po.cancelled"); got != 1 {
		t.Fatalf("po.cancelled rows = %d, want 1", got)
	}

	// Cross-tenant: tenant B's buyer cannot see tenant A's PO (RLS).
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: pinned.GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant issue: code = %v, want NotFound", status.Code(err))
	}
	// No propagated auth metadata: Unauthenticated before any store hit.
	if _, err := client.IssuePurchaseOrder(context.Background(),
		&procurementv1.IssuePurchaseOrderRequest{Id: pinned.GetId()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
}

// The #35 + #36 error mapping as a pure unit test (no database): machine
// codes verbatim in the gRPC message, Aborted for the lifecycle guards
// (gateway renders 409), FailedPrecondition for the vendor gate (412).
func TestMapPurchaseOrderError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
		msg  string
	}{
		"vendor id invalid":      {purchaseorder.ErrVendorIDInvalid, codes.InvalidArgument, "po_vendor_id_invalid"},
		"not approved":           {purchaseorder.ErrRequisitionNotApproved, codes.Aborted, "po_requisition_not_approved"},
		"po id invalid":          {purchaseorder.ErrIDInvalid, codes.InvalidArgument, "po_id_invalid"},
		"po not found":           {purchaseorder.ErrNotFound, codes.NotFound, "po_not_found"},
		"illegal transition":     {purchaseorder.ErrIllegalTransition, codes.Aborted, "po_illegal_transition"},
		"has receipts":           {purchaseorder.ErrHasReceipts, codes.Aborted, "po_has_receipts"},
		"requisition not found":  {requisition.ErrNotFound, codes.NotFound, "requisition_not_found"},
		"requisition id invalid": {requisition.ErrIDInvalid, codes.InvalidArgument, "requisition_id_invalid"},
		"vendor not found":       {vendor.ErrNotFound, codes.NotFound, "vendor_not_found"},
		"vendor deactivated":     {vendor.ErrDeactivated, codes.FailedPrecondition, "vendor_deactivated"},
		"unknown error internal": {errors.New("boom"), codes.Internal, "internal error"},
	} {
		got := mapPurchaseOrderError(tc.err)
		if status.Code(got) != tc.code || status.Convert(got).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)",
				name, status.Code(got), status.Convert(got).Message(), tc.code, tc.msg)
		}
	}
}

// grpcAcknowledgedPO drives the real flow to an acknowledged PO of tenant A
// (rules → approved requisition → convert → issue → acknowledge); the
// returned PO carries its items with ids — the receipt lines' references.
func grpcAcknowledgedPO(t *testing.T, client procurementv1.ProcurementClient) *procurementv1.PurchaseOrder {
	t.Helper()
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	approved := grpcApprovedRequisition(t, client)
	vendorA, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-GRN37-" + approved.GetNumber()[3:], Name: "PT GRN"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	wrapped, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorA.GetId()})
	if err != nil || !wrapped.GetCreated() {
		t.Fatalf("convert = (%v, %v)", wrapped.GetCreated(), err)
	}
	po := wrapped.GetPurchaseOrder()
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: po.GetId()}); err != nil {
		t.Fatalf("issue: %v", err)
	}
	ack, err := client.AcknowledgePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: po.GetId()})
	if err != nil || ack.GetStatus() != "acknowledged" {
		t.Fatalf("acknowledge = (%s, %v)", ack.GetStatus(), err)
	}
	for _, it := range ack.GetItems() {
		if it.GetId() == "" {
			t.Fatalf("acknowledged PO items carry no ids: %+v", ack.GetItems())
		}
	}
	return ack
}

// grpcItemByQty picks the PO line with the given qty_ordered — po_items rows
// share created_at, so load order is not the basket order (dev-note 035).
func grpcItemByQty(t *testing.T, po *procurementv1.PurchaseOrder, qty int32) *procurementv1.PurchaseOrderItem {
	t.Helper()
	for _, it := range po.GetItems() {
		if it.GetQtyOrdered() == qty {
			return it
		}
	}
	t.Fatalf("no PO line with qty_ordered %d in %+v", qty, po.GetItems())
	return nil
}

// grpcCleanupGRNVendor wipes this suite's fixture vendors (code prefix
// G-GRN37-); grpcCleanupPOVendor covers only the #35 prefix.
func grpcCleanupGRNVendor(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'G-GRN37-%'`,
			gTenantA, gTenantB)
		return err
	})
}

// The #37 goods receipts through the full gRPC wiring: RBAC (grn:post is
// buyer-only), the three status branches with exactly one grn.posted outbox
// row each (actor + trace + purchase_order entity), the pinned over-receipt
// detail line, the machine codes of every guard, cross-tenant invisibility,
// and the anonymous rejection.
func TestGoodsReceiptGRPC(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	grpcCleanupPOVendor(t, db)
	grpcCleanupGRNVendor(t, db)
	t.Cleanup(func() {
		grpcCleanupRequisition(t, db)
		grpcCleanupPOVendor(t, db)
		grpcCleanupGRNVendor(t, db)
	})

	po := grpcAcknowledgedPO(t, client)
	laptop := grpcItemByQty(t, po, 2)
	mouse := grpcItemByQty(t, po, 3)

	// grn:post is buyer-only: requester, approver, finance, and viewer are
	// denied at the service-layer re-check.
	for _, role := range []string{rbac.RoleRequester, rbac.RoleApprover, rbac.RoleFinance, rbac.RoleViewer} {
		_, err := client.PostGoodsReceipt(callerCtx(gTenantA, role), &procurementv1.PostGoodsReceiptRequest{
			PoId: po.GetId(),
			Items: []*procurementv1.GoodsReceiptItem{
				{PoItemId: laptop.GetId(), QtyReceived: 1},
			},
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("post as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
	}

	// Machine codes: bad id, unknown PO, empty items — before any write.
	_, err := client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: "nope",
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: laptop.GetId(), QtyReceived: 1},
		},
	})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "po_id_invalid" {
		t.Fatalf("bad po id: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: "dddddddd-0000-0000-0000-0000000000ff",
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: laptop.GetId(), QtyReceived: 1},
		},
	})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "po_not_found" {
		t.Fatalf("unknown po: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: po.GetId(),
	})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "grn_items_empty" {
		t.Fatalf("empty items: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	if got := grpcPOEventCount(t, db, po.GetId(), "grn.posted"); got != 0 {
		t.Fatalf("grn.posted rows after rejections = %d, want 0", got)
	}

	// Over-receipt: the pinned detail line verbatim, nothing written.
	_, err = client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: po.GetId(),
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: laptop.GetId(), QtyReceived: 3},
		},
	})
	want := "grn_over_receipt: po_item=" + laptop.GetId() + " received=0 requested=3 ordered=2"
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != want {
		t.Fatalf("over receipt: got (%v, %q), want (Aborted, %q)", status.Code(err), status.Convert(err).Message(), want)
	}
	if got := grpcPOEventCount(t, db, po.GetId(), "grn.posted"); got != 0 {
		t.Fatalf("grn.posted rows after over-receipt = %d, want 0", got)
	}

	// Illegal status: a draft PO cannot receive. (The conversion response
	// carries no item ids — only the locked loads do — so read the line's id
	// straight from the table for this fixture.)
	approved := grpcApprovedRequisition(t, client)
	vendorB, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-GRN37-D", Name: "PT GRN Draft"})
	if err != nil {
		t.Fatalf("create draft vendor: %v", err)
	}
	draftWrapped, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorB.GetId()})
	if err != nil {
		t.Fatalf("convert draft: %v", err)
	}
	var draftLaptopID string
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT id::text FROM procurement.po_items
			WHERE po_id = $1 AND qty_ordered = 2`, draftWrapped.GetPurchaseOrder().GetId()).Scan(&draftLaptopID)
	}); err != nil {
		t.Fatalf("read draft item id: %v", err)
	}
	_, err = client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: draftWrapped.GetPurchaseOrder().GetId(),
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: draftLaptopID, QtyReceived: 1},
		},
	})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "po_illegal_transition" {
		t.Fatalf("draft receipt: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}

	// Happy path with a trace: first partial → partially_received, GRN
	// number GRN-000001, one grn.posted row (actor + trace + PO entity).
	buyer := metadata.AppendToOutgoingContext(callerCtx(gTenantA, rbac.RoleBuyer), "x-trace-id", "trace-grpc-37")
	first, err := client.PostGoodsReceipt(buyer, &procurementv1.PostGoodsReceiptRequest{
		PoId:  po.GetId(),
		Notes: "dock 3",
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: laptop.GetId(), QtyReceived: 1},
		},
	})
	if err != nil {
		t.Fatalf("post 1: %v", err)
	}
	if grn := first.GetGoodsReceipt(); grn.GetNumber() != "GRN-000001" || grn.GetPoId() != po.GetId() ||
		grn.GetReceivedBy() != gUser || grn.GetNotes() != "dock 3" || len(grn.GetItems()) != 1 ||
		grn.GetItems()[0].GetPoItemId() != laptop.GetId() || grn.GetItems()[0].GetQtyReceived() != 1 {
		t.Fatalf("grn1 = %+v", grn)
	}
	if after := first.GetPurchaseOrder(); after.GetStatus() != "partially_received" {
		t.Fatalf("po after post 1 = %s, want partially_received", after.GetStatus())
	}
	var eventType, entity, actorID, traceID string
	var payload []byte
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT event_type, entity_type, actor_id::text, trace_id, payload
			FROM procurement.outbox WHERE entity_id = $1 AND event_type = 'grn.posted'`,
			po.GetId()).Scan(&eventType, &entity, &actorID, &traceID, &payload)
	}); err != nil {
		t.Fatalf("load grn.posted row: %v", err)
	}
	if eventType != "grn.posted" || entity != "purchase_order" || actorID != gUser || traceID != "trace-grpc-37" {
		t.Fatalf("grn.posted row = (%s, %s, %s, %s)", eventType, entity, actorID, traceID)
	}
	var posted struct {
		GRNID      string `json:"grn_id"`
		Number     string `json:"number"`
		PONumber   string `json:"po_number"`
		POStatus   string `json:"po_status"`
		ReceivedBy string `json:"received_by"`
		Items      []struct {
			POItemID    string `json:"po_item_id"`
			QtyReceived int32  `json:"qty_received"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &posted); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if posted.GRNID != first.GetGoodsReceipt().GetId() || posted.Number != "GRN-000001" ||
		posted.PONumber != po.GetNumber() || posted.POStatus != "partially_received" ||
		posted.ReceivedBy != gUser || len(posted.Items) != 1 ||
		posted.Items[0].POItemID != laptop.GetId() || posted.Items[0].QtyReceived != 1 {
		t.Fatalf("grn.posted payload = %+v", posted)
	}

	// Second partial: same status, second GRN, second outbox row.
	second, err := client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: po.GetId(),
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: mouse.GetId(), QtyReceived: 2},
		},
	})
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if second.GetGoodsReceipt().GetNumber() != "GRN-000002" ||
		second.GetPurchaseOrder().GetStatus() != "partially_received" {
		t.Fatalf("post 2 = (%s, %s)", second.GetGoodsReceipt().GetNumber(), second.GetPurchaseOrder().GetStatus())
	}
	if got := grpcPOEventCount(t, db, po.GetId(), "grn.posted"); got != 2 {
		t.Fatalf("grn.posted rows after post 2 = %d, want 2", got)
	}

	// Completion: every line full → received.
	third, err := client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: po.GetId(),
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: laptop.GetId(), QtyReceived: 1},
			{PoItemId: mouse.GetId(), QtyReceived: 1},
		},
	})
	if err != nil || third.GetPurchaseOrder().GetStatus() != "received" {
		t.Fatalf("post 3 = (%s, %v)", third.GetPurchaseOrder().GetStatus(), err)
	}
	for _, it := range third.GetPurchaseOrder().GetItems() {
		if it.GetQtyReceived() != it.GetQtyOrdered() {
			t.Fatalf("line %s received %d of %d", it.GetId(), it.GetQtyReceived(), it.GetQtyOrdered())
		}
	}

	// A receipt against the received PO answers over-receipt (the cumulative
	// guard fires before the edge guard), not illegal transition.
	_, err = client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: po.GetId(),
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: laptop.GetId(), QtyReceived: 1},
		},
	})
	wantAfter := "grn_over_receipt: po_item=" + laptop.GetId() + " received=2 requested=1 ordered=2"
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != wantAfter {
		t.Fatalf("receipt on received PO: got (%v, %q), want (Aborted, %q)",
			status.Code(err), status.Convert(err).Message(), wantAfter)
	}

	// Cross-tenant: tenant B's buyer cannot even see the PO (RLS).
	_, err = client.PostGoodsReceipt(callerCtx(gTenantB, rbac.RoleBuyer), &procurementv1.PostGoodsReceiptRequest{
		PoId: po.GetId(),
		Items: []*procurementv1.GoodsReceiptItem{
			{PoItemId: laptop.GetId(), QtyReceived: 1},
		},
	})
	if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "po_not_found" {
		t.Fatalf("cross-tenant: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}

	// Anonymous: Unauthenticated before any store hit.
	if _, err := client.PostGoodsReceipt(context.Background(), &procurementv1.PostGoodsReceiptRequest{
		PoId: po.GetId(),
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
}

// The #37 error mapping as a pure unit test (no database): shape codes →
// InvalidArgument, the over-receipt detail line and the illegal-transition
// guard → Aborted, not-found → 404 — the typed error carries its pinned
// message verbatim while errors.Is keeps the sentinel matchable.
func TestMapGoodsReceiptError(t *testing.T) {
	typed := &purchaseorder.OverReceiptError{
		POItemID: "55555555-0000-0000-0000-0000000000a1", Received: 1, Requested: 2, Ordered: 2,
	}
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
		msg  string
	}{
		"items empty":        {goodsreceipt.ErrItemsEmpty, codes.InvalidArgument, "grn_items_empty"},
		"qty invalid":        {goodsreceipt.ErrQtyInvalid, codes.InvalidArgument, "grn_qty_invalid"},
		"line duplicate":     {goodsreceipt.ErrPOItemDuplicate, codes.InvalidArgument, "grn_po_item_duplicate"},
		"notes invalid":      {goodsreceipt.ErrNotesInvalid, codes.InvalidArgument, "grn_notes_invalid"},
		"line unknown":       {purchaseorder.ErrReceiptItemUnknown, codes.InvalidArgument, "grn_po_item_unknown"},
		"po id invalid":      {purchaseorder.ErrIDInvalid, codes.InvalidArgument, "po_id_invalid"},
		"illegal transition": {purchaseorder.ErrIllegalTransition, codes.Aborted, "po_illegal_transition"},
		"po not found":       {purchaseorder.ErrNotFound, codes.NotFound, "po_not_found"},
		"over receipt typed": {typed, codes.Aborted,
			"grn_over_receipt: po_item=55555555-0000-0000-0000-0000000000a1 received=1 requested=2 ordered=2"},
		"over receipt bare": {purchaseorder.ErrOverReceipt, codes.Aborted, "grn_over_receipt"},
		"unknown internal":  {errors.New("boom"), codes.Internal, "internal error"},
	} {
		got := mapGoodsReceiptError(tc.err)
		if status.Code(got) != tc.code || status.Convert(got).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)",
				name, status.Code(got), status.Convert(got).Message(), tc.code, tc.msg)
		}
	}
}

// grpcInvoicablePO drives the real flow to an acknowledged PO of tenant A
// (rules → approved requisition → convert → issue → acknowledge) under the
// given fixture vendor code — the invoice tests' own prefix, wiped by
// grpcCleanupInvoiceVendor.
func grpcInvoicablePO(t *testing.T, client procurementv1.ProcurementClient, code string) *procurementv1.PurchaseOrder {
	t.Helper()
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	approved := grpcApprovedRequisition(t, client)
	vendorA, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: code, Name: "PT Invoice"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	wrapped, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorA.GetId()})
	if err != nil || !wrapped.GetCreated() {
		t.Fatalf("convert = (%v, %v)", wrapped.GetCreated(), err)
	}
	po := wrapped.GetPurchaseOrder()
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: po.GetId()}); err != nil {
		t.Fatalf("issue: %v", err)
	}
	ack, err := client.AcknowledgePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: po.GetId()})
	if err != nil || ack.GetStatus() != "acknowledged" {
		t.Fatalf("acknowledge = (%s, %v)", ack.GetStatus(), err)
	}
	return ack
}

// grpcCleanupInvoiceVendor wipes this suite's fixture vendors (prefix
// G-INV39-).
func grpcCleanupInvoiceVendor(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'G-INV39-%'`,
			gTenantA, gTenantB)
		return err
	})
}

// grpcInvoiceEventCount counts the tenants' invoice.* outbox rows — create
// writes none (#39); matched/discrepancy belong to #40.
func grpcInvoiceEventCount(t *testing.T, db *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.outbox WHERE tenant_id IN ($1, $2) AND event_type LIKE 'invoice.%'`,
			gTenantA, gTenantB).Scan(&n)
	}); err != nil {
		t.Fatalf("count invoice events: %v", err)
	}
	return n
}

// The #39 invoices through the full gRPC wiring: RBAC (invoice:write is
// finance/admin only — the buyer is denied, unlike grn:post), the derived
// vendor/currency, the server-side total, the pinned over-bill detail line,
// the machine codes of every guard, the duplicate rule per (tenant, vendor),
// no invoice.* outbox rows, no PO mutation, cross-tenant invisibility, and
// the anonymous rejection.
func TestInvoiceGRPC(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	grpcCleanupPOVendor(t, db)
	grpcCleanupGRNVendor(t, db)
	grpcCleanupInvoiceVendor(t, db)
	t.Cleanup(func() {
		grpcCleanupRequisition(t, db)
		grpcCleanupPOVendor(t, db)
		grpcCleanupGRNVendor(t, db)
		grpcCleanupInvoiceVendor(t, db)
	})

	po := grpcInvoicablePO(t, client, "G-INV39-A")
	laptop := grpcItemByQty(t, po, 2)
	mouse := grpcItemByQty(t, po, 3)

	// invoice:write is finance/admin: requester, buyer, approver, and viewer
	// are denied at the service-layer re-check.
	for _, role := range []string{rbac.RoleRequester, rbac.RoleBuyer, rbac.RoleApprover, rbac.RoleViewer} {
		_, err := client.CreateInvoice(callerCtx(gTenantA, role), &procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: "INV-RBAC",
			Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 1}},
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("create as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
	}

	// Machine codes: bad po id, unknown PO, empty number, empty items, bad
	// line shapes, unknown line — before any write.
	for name, tc := range map[string]struct {
		req  *procurementv1.CreateInvoiceRequest
		code codes.Code
		msg  string
	}{
		"bad po id": {&procurementv1.CreateInvoiceRequest{
			PoId: "nope", Number: "INV-1",
			Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 1}},
		}, codes.InvalidArgument, "po_id_invalid"},
		"unknown po": {&procurementv1.CreateInvoiceRequest{
			PoId: "dddddddd-0000-0000-0000-0000000000ff", Number: "INV-1",
			Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 1}},
		}, codes.NotFound, "po_not_found"},
		"empty number": {&procurementv1.CreateInvoiceRequest{
			PoId:  po.GetId(),
			Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 1}},
		}, codes.InvalidArgument, "inv_number_invalid"},
		"empty items": {&procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: "INV-1",
		}, codes.InvalidArgument, "inv_items_empty"},
		"zero qty": {&procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: "INV-1",
			Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 0, UnitPriceCents: 1}},
		}, codes.InvalidArgument, "inv_qty_invalid"},
		"duplicate line": {&procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: "INV-1",
			Items: []*procurementv1.InvoiceItem{
				{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 1},
				{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 1},
			},
		}, codes.InvalidArgument, "inv_po_item_duplicate"},
		"unknown line": {&procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: "INV-1",
			Items: []*procurementv1.InvoiceItem{{PoItemId: "dddddddd-0000-0000-0000-0000000000ee", Qty: 1, UnitPriceCents: 1}},
		}, codes.InvalidArgument, "inv_po_item_unknown"},
	} {
		_, err := client.CreateInvoice(callerCtx(gTenantA, rbac.RoleFinance), tc.req)
		if status.Code(err) != tc.code || status.Convert(err).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", name,
				status.Code(err), status.Convert(err).Message(), tc.code, tc.msg)
		}
	}

	// Over-bill: 2 × 8,000,000 > the 15,450,000 PO total — the pinned detail
	// line verbatim, nothing written.
	_, err := client.CreateInvoice(callerCtx(gTenantA, rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.GetId(), Number: "INV-OVER",
		Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 2, UnitPriceCents: 8000000}},
	})
	want := "inv_over_bill: po_number=" + po.GetNumber() + " invoiced=0 requested=16000000 total=15450000"
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != want {
		t.Fatalf("over bill: got (%v, %q), want (Aborted, %q)", status.Code(err), status.Convert(err).Message(), want)
	}

	// Illegal status: a draft PO cannot be invoiced.
	approved := grpcApprovedRequisition(t, client)
	vendorB, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-INV39-D", Name: "PT Invoice Draft"})
	if err != nil {
		t.Fatalf("create draft vendor: %v", err)
	}
	draftWrapped, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: approved.GetId(), VendorId: vendorB.GetId()})
	if err != nil {
		t.Fatalf("convert draft: %v", err)
	}
	var draftLaptopID string
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT id::text FROM procurement.po_items
			WHERE po_id = $1 AND qty_ordered = 2`, draftWrapped.GetPurchaseOrder().GetId()).Scan(&draftLaptopID)
	}); err != nil {
		t.Fatalf("read draft item id: %v", err)
	}
	_, err = client.CreateInvoice(callerCtx(gTenantA, rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: draftWrapped.GetPurchaseOrder().GetId(), Number: "INV-DRAFT",
		Items: []*procurementv1.InvoiceItem{{PoItemId: draftLaptopID, Qty: 1, UnitPriceCents: 400000}},
	})
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "po_illegal_transition" {
		t.Fatalf("draft invoice: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}

	// Happy path: finance creates against the acknowledged PO — vendor and
	// currency derived, total recomputed, status pending, no match result.
	created, err := client.CreateInvoice(callerCtx(gTenantA, rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.GetId(), Number: " INV/2026/0001 ",
		Items: []*procurementv1.InvoiceItem{
			{PoItemId: laptop.GetId(), Qty: 2, UnitPriceCents: 7500000},
			{PoItemId: mouse.GetId(), Qty: 1, UnitPriceCents: 150000},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.GetNumber() != "INV/2026/0001" || created.GetPoId() != po.GetId() ||
		created.GetVendorId() != po.GetVendorId() || created.GetCurrency() != "IDR" ||
		created.GetTotalCents() != 15150000 || created.GetStatus() != "pending" ||
		created.GetMatchResultJson() != "" || created.GetCreatedBy() != gUser || created.GetCreatedAt() == "" {
		t.Fatalf("invoice = %+v", created)
	}
	if len(created.GetItems()) != 2 {
		t.Fatalf("items = %+v", created.GetItems())
	}
	// No invoice.* outbox rows; the PO stays acknowledged and untouched.
	if got := grpcInvoiceEventCount(t, db); got != 0 {
		t.Fatalf("invoice.* outbox rows after create = %d, want 0", got)
	}

	// Duplicate number for the same vendor → AlreadyExists.
	_, err = client.CreateInvoice(callerCtx(gTenantA, rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.GetId(), Number: "INV/2026/0001",
		Items: []*procurementv1.InvoiceItem{{PoItemId: mouse.GetId(), Qty: 1, UnitPriceCents: 150000}},
	})
	if status.Code(err) != codes.AlreadyExists || status.Convert(err).Message() != "inv_number_duplicate" {
		t.Fatalf("duplicate: got (%v, %q), want AlreadyExists inv_number_duplicate", status.Code(err), status.Convert(err).Message())
	}

	// GetInvoice: the detail read with lines (order is by created_at, id —
	// the lines share created_at, so assert membership, not position); bad
	// id; cross-tenant 404.
	got, err := client.GetInvoice(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.GetInvoiceRequest{Id: created.GetId()})
	if err != nil || got.GetId() != created.GetId() || len(got.GetItems()) != 2 {
		t.Fatalf("get = (%+v, %v)", got, err)
	}
	gotLaptop := false
	for _, it := range got.GetItems() {
		if it.GetPoItemId() == laptop.GetId() && it.GetQty() == 2 && it.GetUnitPriceCents() == 7500000 {
			gotLaptop = true
		}
	}
	if !gotLaptop {
		t.Fatalf("get items carry no laptop line: %+v", got.GetItems())
	}
	if _, err := client.GetInvoice(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.GetInvoiceRequest{Id: "nope"}); status.Code(err) != codes.InvalidArgument ||
		status.Convert(err).Message() != "inv_id_invalid" {
		t.Fatalf("get bad id: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	if _, err := client.GetInvoice(callerCtx(gTenantB, rbac.RoleViewer),
		&procurementv1.GetInvoiceRequest{Id: created.GetId()}); status.Code(err) != codes.NotFound ||
		status.Convert(err).Message() != "inv_not_found" {
		t.Fatalf("cross-tenant get: got (%v, %q), want NotFound inv_not_found", status.Code(err), status.Convert(err).Message())
	}

	// Anonymous: Unauthenticated before any store hit.
	if _, err := client.CreateInvoice(context.Background(), &procurementv1.CreateInvoiceRequest{
		PoId: po.GetId(), Number: "INV-ANON",
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
}

// grpcSetInvoiceStatus is the #41 list fixture's SQL half: only the #40 match
// writes matched/discrepancy, and the list only needs the stored values.
func grpcSetInvoiceStatus(t *testing.T, db *pgxpool.Pool, invoiceID, status string) {
	t.Helper()
	if err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			UPDATE procurement.invoices SET status = $2 WHERE id = $1`, invoiceID, status)
		return err
	}); err != nil {
		t.Fatalf("set invoice status %s: %v", status, err)
	}
}

// The #41 invoice list through the full gRPC wiring (the #38 PO-list twin):
// defaults newest-first with header-only rows (no items, no match_result —
// even on invoices whose stored verdict landed), the three-status whitelist,
// the case-insensitive number prefix, combined filters, the past-the-end
// total fallback, cross-tenant invisibility, the list machine codes, and the
// anonymous rejection. Listed by a viewer — invoice:read is every role.
func TestInvoiceGRPCList(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	grpcCleanupPOVendor(t, db)
	grpcCleanupInvoiceVendor(t, db)
	defer func() { // LIFO: before the pool closes (the file's leak pattern).
		grpcCleanupRequisition(t, db)
		grpcCleanupPOVendor(t, db)
		grpcCleanupInvoiceVendor(t, db)
	}()

	// Three invoices of one vendor in tenant A (3 × 5,000,000 fits the
	// 15,450,000 budget), mixed-case numbers for the prefix arm.
	po := grpcInvoicablePO(t, client, "G-INV39-A")
	laptop := grpcItemByQty(t, po, 2)
	create := func(number string) *procurementv1.Invoice {
		t.Helper()
		inv, err := client.CreateInvoice(callerCtx(gTenantA, rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
			PoId: po.GetId(), Number: number,
			Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 5000000}},
		})
		if err != nil {
			t.Fatalf("create %s: %v", number, err)
		}
		return inv
	}
	first := create("inv/lowercase/001")
	second := create("INV/MIXED/002")
	third := create("Inv/Mixed/003")
	grpcSetInvoiceStatus(t, db, second.GetId(), "discrepancy")
	grpcSetInvoiceStatus(t, db, third.GetId(), "matched")

	// Tenant B's own invoice — the cross-tenant row.
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantB, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules B: %v", err)
	}
	vendorB, err := client.CreateVendor(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-INV39-B", Name: "PT Invoice B"})
	if err != nil {
		t.Fatalf("create vendor B: %v", err)
	}
	prB, err := client.CreateRequisition(callerCtx(gTenantB, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	submittedB, err := client.SubmitRequisition(callerCtx(gTenantB, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: prB.GetId()})
	if err != nil {
		t.Fatalf("submit B: %v", err)
	}
	if _, err := client.DecideApproval(callerCtxAs(gUserB, gTenantB, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: submittedB.GetApprovalSteps()[0].GetId(), Decision: "approve"}); err != nil {
		t.Fatalf("decide B: %v", err)
	}
	wrappedB, err := client.CreatePurchaseOrder(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: prB.GetId(), VendorId: vendorB.GetId()})
	if err != nil {
		t.Fatalf("convert B: %v", err)
	}
	poB := wrappedB.GetPurchaseOrder()
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: poB.GetId()}); err != nil {
		t.Fatalf("issue B: %v", err)
	}
	ackB, err := client.AcknowledgePurchaseOrder(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.AcknowledgePurchaseOrderRequest{Id: poB.GetId()})
	if err != nil {
		t.Fatalf("acknowledge B: %v", err)
	}
	invB, err := client.CreateInvoice(callerCtx(gTenantB, rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: poB.GetId(), Number: "INV-TENANT-B",
		Items: []*procurementv1.InvoiceItem{{PoItemId: grpcItemByQty(t, ackB, 2).GetId(), Qty: 1, UnitPriceCents: 5000000}},
	})
	if err != nil {
		t.Fatalf("create B invoice: %v", err)
	}

	// Defaults: page 1, page size 20, tenant A's three rows newest first —
	// listed by a viewer (invoice:read is broad; writes are finance-only).
	resp, err := client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer), &procurementv1.ListInvoicesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.GetPage() != 1 || resp.GetPageSize() != 20 || resp.GetTotal() != 3 || len(resp.GetInvoices()) != 3 {
		t.Fatalf("defaults: page=%d size=%d total=%d rows=%d",
			resp.GetPage(), resp.GetPageSize(), resp.GetTotal(), len(resp.GetInvoices()))
	}
	rows := resp.GetInvoices()
	if rows[0].GetId() != third.GetId() || rows[2].GetId() != first.GetId() {
		t.Fatalf("not newest-first: %s...%s", rows[0].GetId(), rows[2].GetId())
	}
	// Header-only: items and match_result are never populated on list rows —
	// even on the matched/discrepancy rows whose stored verdicts exist.
	for _, row := range rows {
		if len(row.GetItems()) != 0 {
			t.Fatalf("list row carries items: %+v", row)
		}
		if row.GetMatchResultJson() != "" {
			t.Fatalf("list row carries match_result: %+v", row)
		}
		if row.GetId() == "" || row.GetTenantId() != gTenantA || row.GetVendorId() == "" ||
			row.GetPoId() == "" || row.GetNumber() == "" || row.GetCurrency() == "" ||
			row.GetTotalCents() == 0 || row.GetStatus() == "" || row.GetCreatedAt() == "" {
			t.Fatalf("header fields missing: %+v", row)
		}
	}

	// Status filters over the three-value whitelist.
	resp, err = client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListInvoicesRequest{Status: "matched", PageSize: 10})
	if err != nil || resp.GetTotal() != 1 || resp.GetInvoices()[0].GetId() != third.GetId() {
		t.Fatalf("status filter = (%+v, %v)", resp, err)
	}
	resp, err = client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListInvoicesRequest{Status: "pending", PageSize: 10})
	if err != nil || resp.GetTotal() != 1 || resp.GetInvoices()[0].GetId() != first.GetId() {
		t.Fatalf("pending filter = (%+v, %v)", resp, err)
	}
	// Number-prefix query, lowercase input over mixed-case numbers.
	resp, err = client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListInvoicesRequest{Query: "inv/lower"})
	if err != nil || resp.GetTotal() != 1 || resp.GetInvoices()[0].GetId() != first.GetId() {
		t.Fatalf("prefix query = (%+v, %v)", resp, err)
	}
	// Combined: the shared prefix + matched leaves only third.
	resp, err = client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListInvoicesRequest{Query: "INV/", Status: "matched", PageSize: 10})
	if err != nil || resp.GetTotal() != 1 || resp.GetInvoices()[0].GetId() != third.GetId() {
		t.Fatalf("combined = (%+v, %v)", resp, err)
	}
	// Past-the-end page: empty rows, true total via the COUNT fallback.
	resp, err = client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListInvoicesRequest{Page: 9, PageSize: 2})
	if err != nil || len(resp.GetInvoices()) != 0 || resp.GetTotal() != 3 {
		t.Fatalf("past-the-end = (%+v, %v)", resp, err)
	}
	// Cross-tenant listing: tenant B sees only its own row.
	resp, err = client.ListInvoices(callerCtx(gTenantB, rbac.RoleViewer), &procurementv1.ListInvoicesRequest{})
	if err != nil || resp.GetTotal() != 1 || resp.GetInvoices()[0].GetTenantId() != gTenantB ||
		resp.GetInvoices()[0].GetId() != invB.GetId() {
		t.Fatalf("tenantB sees (%+v, %v)", resp, err)
	}

	// Validation: unknown status and absurd page answer the #41 machine codes
	// as InvalidArgument.
	_, err = client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListInvoicesRequest{Status: "paused"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "inv_status_invalid" {
		t.Fatalf("bad status: got (%v, %q), want InvalidArgument inv_status_invalid", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.ListInvoices(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.ListInvoicesRequest{Page: 2000000})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "inv_page_invalid" {
		t.Fatalf("bad page: got (%v, %q), want InvalidArgument inv_page_invalid", status.Code(err), status.Convert(err).Message())
	}

	// Anonymous: the interceptor's re-check rejects metadata-less calls (the
	// method must stay in methodPermissions).
	if _, err := client.ListInvoices(context.Background(), &procurementv1.ListInvoicesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous list: code = %v, want Unauthenticated", status.Code(err))
	}
}

// The #39 error mapping as a pure unit test (no database): shape codes →
// InvalidArgument, the over-bill detail line and the illegal-status guard →
// Aborted, duplicate number → AlreadyExists, not-found → 404 — the typed
// error carries its pinned message verbatim while errors.Is keeps the
// sentinel matchable.
func TestMapInvoiceError(t *testing.T) {
	typed := &invoice.OverBillError{
		PONumber: "PO-000001", Invoiced: 6000000, Requested: 6000000, Total: 10000000,
	}
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
		msg  string
	}{
		"number invalid":     {invoice.ErrNumberInvalid, codes.InvalidArgument, "inv_number_invalid"},
		"items empty":        {invoice.ErrItemsEmpty, codes.InvalidArgument, "inv_items_empty"},
		"qty invalid":        {invoice.ErrQtyInvalid, codes.InvalidArgument, "inv_qty_invalid"},
		"price invalid":      {invoice.ErrUnitPriceInvalid, codes.InvalidArgument, "inv_unit_price_invalid"},
		"line duplicate":     {invoice.ErrPOItemDuplicate, codes.InvalidArgument, "inv_po_item_duplicate"},
		"line unknown":       {invoice.ErrPOItemUnknown, codes.InvalidArgument, "inv_po_item_unknown"},
		"invoice id invalid": {invoice.ErrIDInvalid, codes.InvalidArgument, "inv_id_invalid"},
		"status invalid":     {invoice.ErrStatusInvalid, codes.InvalidArgument, "inv_status_invalid"},
		"page invalid":       {invoice.ErrPageInvalid, codes.InvalidArgument, "inv_page_invalid"},
		"po id invalid":      {purchaseorder.ErrIDInvalid, codes.InvalidArgument, "po_id_invalid"},
		"duplicate number":   {invoice.ErrNumberDuplicate, codes.AlreadyExists, "inv_number_duplicate"},
		"already matched":    {invoice.ErrAlreadyMatched, codes.Aborted, "inv_already_matched"},
		"illegal transition": {purchaseorder.ErrIllegalTransition, codes.Aborted, "po_illegal_transition"},
		"invoice not found":  {invoice.ErrNotFound, codes.NotFound, "inv_not_found"},
		"po not found":       {purchaseorder.ErrNotFound, codes.NotFound, "po_not_found"},
		"over bill typed": {typed, codes.Aborted,
			"inv_over_bill: po_number=PO-000001 invoiced=6000000 requested=6000000 total=10000000"},
		"over bill bare":   {invoice.ErrOverBill, codes.Aborted, "inv_over_bill"},
		"unknown internal": {errors.New("boom"), codes.Internal, "internal error"},
	} {
		got := mapInvoiceError(tc.err)
		if status.Code(got) != tc.code || status.Convert(got).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)",
				name, status.Code(got), status.Convert(got).Message(), tc.code, tc.msg)
		}
	}
}

// grpcCleanupMatchSettings wipes this suite's tolerance rows and fixture
// vendors (the 0010 table has no FK neighbors; the G-MT40- vendors belong
// to the match tests alone).
func grpcCleanupMatchSettings(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx,
			`DELETE FROM procurement.match_settings WHERE tenant_id IN ($1, $2)`, gTenantA, gTenantB); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM procurement.vendors WHERE tenant_id IN ($1, $2) AND code LIKE 'G-MT40-%'`, gTenantA, gTenantB)
		return err
	})
}

// The #40 match through the full gRPC wiring: RBAC (MatchInvoice is
// invoice:write — finance/admin; the buyer and viewer are denied), the
// status move pending → discrepancy → matched with the late-GRN resolution
// loop, exactly one invoice.* outbox event per successful run, the matched
// re-run guard (Aborted inv_already_matched), the machine codes, and the
// admin-only settings surface (match:configure denies finance).
func TestMatchGRPC(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	grpcCleanupInvoiceVendor(t, db)
	grpcCleanupMatchSettings(t, db)
	t.Cleanup(func() {
		grpcCleanupRequisition(t, db)
		grpcCleanupInvoiceVendor(t, db)
		grpcCleanupMatchSettings(t, db)
	})

	po := grpcInvoicablePO(t, client, "G-MT40-A")
	laptop := grpcItemByQty(t, po, 2)

	// invoice:write is finance/admin: requester, buyer, approver, and viewer
	// are denied at the service-layer re-check (buyer can post GRNs but not
	// match; viewer reads but cannot act).
	created, err := client.CreateInvoice(callerCtx(gTenantA, rbac.RoleFinance), &procurementv1.CreateInvoiceRequest{
		PoId: po.GetId(), Number: "INV-MT40-1",
		Items: []*procurementv1.InvoiceItem{{PoItemId: laptop.GetId(), Qty: 1, UnitPriceCents: 7500000}},
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	for _, role := range []string{rbac.RoleRequester, rbac.RoleBuyer, rbac.RoleApprover, rbac.RoleViewer} {
		_, err := client.MatchInvoice(callerCtx(gTenantA, role), &procurementv1.MatchInvoiceRequest{Id: created.GetId()})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("match as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
	}

	// Machine codes: bad id, missing id — before any write.
	if _, err := client.MatchInvoice(callerCtx(gTenantA, rbac.RoleFinance),
		&procurementv1.MatchInvoiceRequest{Id: "nope"}); status.Code(err) != codes.InvalidArgument ||
		status.Convert(err).Message() != "inv_id_invalid" {
		t.Fatalf("bad id: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	if _, err := client.MatchInvoice(callerCtx(gTenantA, rbac.RoleFinance),
		&procurementv1.MatchInvoiceRequest{Id: "dddddddd-0000-0000-0000-0000000000ff"}); status.Code(err) != codes.NotFound ||
		status.Convert(err).Message() != "inv_not_found" {
		t.Fatalf("missing id: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}

	// First run: no receipt exists yet — the billed line flags discrepancy
	// with the pinned qty reason, exactly one invoice.discrepancy event.
	discrepant, err := client.MatchInvoice(callerCtx(gTenantA, rbac.RoleFinance),
		&procurementv1.MatchInvoiceRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if discrepant.GetStatus() != "discrepancy" {
		t.Fatalf("status = %s, want discrepancy", discrepant.GetStatus())
	}
	if !strings.Contains(discrepant.GetMatchResultJson(), "inv_over_bill_qty") ||
		!strings.Contains(discrepant.GetMatchResultJson(), `"qty_received":0`) {
		t.Fatalf("match_result = %s, want the qty breach detail", discrepant.GetMatchResultJson())
	}
	if got := grpcInvoiceEventCount(t, db); got != 1 {
		t.Fatalf("invoice.* events after first run = %d, want 1", got)
	}

	// The late GRN: the buyer posts the receipt, the finance re-run resolves
	// the discrepancy — history stays append-only (two events by now).
	if _, err := client.PostGoodsReceipt(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.PostGoodsReceiptRequest{
			PoId:  po.GetId(),
			Items: []*procurementv1.GoodsReceiptItem{{PoItemId: laptop.GetId(), QtyReceived: 1}},
		}); err != nil {
		t.Fatalf("post grn: %v", err)
	}
	matched, err := client.MatchInvoice(callerCtx(gTenantA, rbac.RoleFinance),
		&procurementv1.MatchInvoiceRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("resolve re-run: %v", err)
	}
	if matched.GetStatus() != "matched" || !strings.Contains(matched.GetMatchResultJson(), `"matched":true`) {
		t.Fatalf("resolved = (%s, %s), want matched", matched.GetStatus(), matched.GetMatchResultJson())
	}
	if got := grpcInvoiceEventCount(t, db); got != 2 {
		t.Fatalf("invoice.* events after resolution = %d, want 2", got)
	}

	// The re-run guard: matched is final — Aborted inv_already_matched, no
	// third event.
	if _, err := client.MatchInvoice(callerCtx(gTenantA, rbac.RoleFinance),
		&procurementv1.MatchInvoiceRequest{Id: created.GetId()}); status.Code(err) != codes.Aborted ||
		status.Convert(err).Message() != "inv_already_matched" {
		t.Fatalf("re-run: got (%v, %q), want Aborted inv_already_matched", status.Code(err), status.Convert(err).Message())
	}
	if got := grpcInvoiceEventCount(t, db); got != 2 {
		t.Fatalf("invoice.* events after rejected re-run = %d, want 2", got)
	}

	// Cross-tenant: the other tenant's finance reads nothing.
	if _, err := client.MatchInvoice(callerCtx(gTenantB, rbac.RoleFinance),
		&procurementv1.MatchInvoiceRequest{Id: created.GetId()}); status.Code(err) != codes.NotFound ||
		status.Convert(err).Message() != "inv_not_found" {
		t.Fatalf("cross-tenant: got (%v, %q), want NotFound inv_not_found", status.Code(err), status.Convert(err).Message())
	}

	// The settings surface: match:configure is admin-only — finance (the
	// money role), buyer, and viewer are all denied.
	for _, role := range []string{rbac.RoleFinance, rbac.RoleBuyer, rbac.RoleViewer} {
		if _, err := client.GetMatchSettings(callerCtx(gTenantA, role),
			&procurementv1.GetMatchSettingsRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("get settings as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
		if _, err := client.ReplaceMatchSettings(callerCtx(gTenantA, role),
			&procurementv1.ReplaceMatchSettingsRequest{QtyPct: 1, PricePct: 1}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("replace settings as %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
	}

	// Admin reads the effective defaults (no row yet), replaces, and reads
	// the stored values back; bounds answer the machine codes.
	def, err := client.GetMatchSettings(callerCtx(gTenantA, rbac.RoleAdmin), &procurementv1.GetMatchSettingsRequest{})
	if err != nil || def.GetQtyPct() != 0 || def.GetPricePct() != 0 {
		t.Fatalf("default settings = (%+v, %v), want 0/0", def, err)
	}
	if _, err := client.ReplaceMatchSettings(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceMatchSettingsRequest{QtyPct: -1}); status.Code(err) != codes.InvalidArgument ||
		status.Convert(err).Message() != "match_qty_pct_invalid" {
		t.Fatalf("bad qty pct: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	if _, err := client.ReplaceMatchSettings(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceMatchSettingsRequest{QtyPct: 0, PricePct: 101}); status.Code(err) != codes.InvalidArgument ||
		status.Convert(err).Message() != "match_price_pct_invalid" {
		t.Fatalf("bad price pct: got (%v, %q)", status.Code(err), status.Convert(err).Message())
	}
	stored, err := client.ReplaceMatchSettings(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceMatchSettingsRequest{QtyPct: 5, PricePct: 2})
	if err != nil || stored.GetQtyPct() != 5 || stored.GetPricePct() != 2 {
		t.Fatalf("replace = (%+v, %v)", stored, err)
	}
	again, err := client.GetMatchSettings(callerCtx(gTenantA, rbac.RoleAdmin), &procurementv1.GetMatchSettingsRequest{})
	if err != nil || again.GetQtyPct() != 5 || again.GetPricePct() != 2 {
		t.Fatalf("get after replace = (%+v, %v)", again, err)
	}

	// Anonymous: Unauthenticated before any store hit.
	if _, err := client.MatchInvoice(context.Background(),
		&procurementv1.MatchInvoiceRequest{Id: created.GetId()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous match: code = %v, want Unauthenticated", status.Code(err))
	}
	if _, err := client.GetMatchSettings(context.Background(),
		&procurementv1.GetMatchSettingsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous settings: code = %v, want Unauthenticated", status.Code(err))
	}
}

// The #40 settings error mapping as a pure unit test (no database).
func TestMapMatchError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
		msg  string
	}{
		"qty pct invalid":   {match.ErrQtyPctInvalid, codes.InvalidArgument, "match_qty_pct_invalid"},
		"price pct invalid": {match.ErrPricePctInvalid, codes.InvalidArgument, "match_price_pct_invalid"},
		"unknown internal":  {errors.New("boom"), codes.Internal, "internal error"},
	} {
		got := mapMatchError(tc.err)
		if status.Code(got) != tc.code || status.Convert(got).Message() != tc.msg {
			t.Errorf("%s: got (%v, %q), want (%v, %q)",
				name, status.Code(got), status.Convert(got).Message(), tc.code, tc.msg)
		}
	}
}

// The #38 read pair through the full gRPC wiring: the list serves
// header-only rows newest-first with the filter contract (po:read — every
// role; a requester may list where the writes are buyer-only), the detail
// owns the item bodies with their ids, cross-tenant ids are
// po_not_found/NotFound, bad filter values answer the po_status_invalid /
// po_page_invalid machine codes, and metadata-less calls are rejected by the
// interceptor's re-check (the methods must stay in methodPermissions).
func TestPurchaseOrderGRPCListAndGet(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	// The final wipe is a defer registered AFTER `defer cleanup()`, so LIFO
	// runs it BEFORE the pool closes (the file's usual t.Cleanup wipes run
	// after `cleanup()` has closed the pool and fail silently — the
	// pre-existing leak pattern other tests absorb via their leading wipes;
	// this test is the file's last, so nothing after it absorbs its residue).
	defer func() {
		grpcCleanupRequisition(t, db)
		grpcCleanupPOVendor(t, db)
	}()
	grpcCleanupRequisition(t, db)
	grpcCleanupPOVendor(t, db)

	// Rules + a vendor, then three POs in tenant A (PO-000001..3); the oldest
	// is issued so the status shape has a value.
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	vendorA, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-PO35-38A", Name: "PT PO38"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	convert := func(t *testing.T, tenant string) *procurementv1.PurchaseOrder {
		wrapped, err := client.CreatePurchaseOrder(callerCtx(tenant, rbac.RoleBuyer),
			&procurementv1.CreatePurchaseOrderRequest{RequisitionId: newApprovedID(t, client), VendorId: vendorA.GetId()})
		if err != nil {
			t.Fatalf("convert %s: %v", tenant, err)
		}
		return wrapped.GetPurchaseOrder()
	}
	first := convert(t, gTenantA)
	second := convert(t, gTenantA)
	third := convert(t, gTenantA)
	// Tenant B needs its own vendor (vendor FK) and rule set — the
	// cross-tenant listing row.
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantB, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules B: %v", err)
	}
	vendorB, err := client.CreateVendor(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: "G-PO35-38B", Name: "PT PO38 B"})
	if err != nil {
		t.Fatalf("create vendor B: %v", err)
	}
	prB, err := client.CreateRequisition(callerCtx(gTenantB, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	submittedB, err := client.SubmitRequisition(callerCtx(gTenantB, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: prB.GetId()})
	if err != nil {
		t.Fatalf("submit B: %v", err)
	}
	if _, err := client.DecideApproval(callerCtxAs(gUserB, gTenantB, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: submittedB.GetApprovalSteps()[0].GetId(), Decision: "approve"}); err != nil {
		t.Fatalf("decide B: %v", err)
	}
	wrappedB, err := client.CreatePurchaseOrder(callerCtx(gTenantB, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: prB.GetId(), VendorId: vendorB.GetId()})
	if err != nil {
		t.Fatalf("convert B: %v", err)
	}
	poB := wrappedB.GetPurchaseOrder()
	if poB.GetTenantId() != gTenantB {
		t.Fatalf("tenant B PO = %+v", poB)
	}
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: first.GetId()}); err != nil {
		t.Fatalf("issue first: %v", err)
	}

	// Defaults: page 1, page size 20, tenant A's three rows newest first —
	// listed by a requester (po:read is broad; the writes are buyer-only).
	resp, err := client.ListPurchaseOrders(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.GetPage() != 1 || resp.GetPageSize() != 20 || resp.GetTotal() != 3 || len(resp.GetPurchaseOrders()) != 3 {
		t.Fatalf("defaults: page=%d size=%d total=%d rows=%d",
			resp.GetPage(), resp.GetPageSize(), resp.GetTotal(), len(resp.GetPurchaseOrders()))
	}
	rows := resp.GetPurchaseOrders()
	if rows[0].GetNumber() != third.GetNumber() || rows[2].GetNumber() != first.GetNumber() {
		t.Fatalf("not newest-first: %s...%s", rows[0].GetNumber(), rows[2].GetNumber())
	}
	// Header-only: items are never populated on list rows; headers carry the
	// linkage fields the UI needs.
	for _, po := range rows {
		if len(po.GetItems()) != 0 {
			t.Fatalf("list row carries items: %+v", po)
		}
		if po.GetId() == "" || po.GetTenantId() != gTenantA || po.GetVendorId() == "" || po.GetRequisitionId() == "" {
			t.Fatalf("header fields missing: %+v", po)
		}
	}

	// Status filter: only the issued row (first).
	resp, err = client.ListPurchaseOrders(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{Status: "issued", PageSize: 10})
	if err != nil || resp.GetTotal() != 1 || resp.GetPurchaseOrders()[0].GetId() != first.GetId() {
		t.Fatalf("status filter = (%+v, %v)", resp, err)
	}
	// Number-prefix query, case-insensitive input: matches second only.
	resp, err = client.ListPurchaseOrders(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{Query: strings.ToLower(second.GetNumber())})
	if err != nil || resp.GetTotal() != 1 || resp.GetPurchaseOrders()[0].GetId() != second.GetId() {
		t.Fatalf("prefix query = (%+v, %v)", resp, err)
	}
	// Combined: the common prefix + draft → second and third, newest first.
	resp, err = client.ListPurchaseOrders(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{Query: "PO-", Status: "draft", Page: 1, PageSize: 1})
	if err != nil || resp.GetTotal() != 2 || len(resp.GetPurchaseOrders()) != 1 ||
		resp.GetPurchaseOrders()[0].GetId() != third.GetId() {
		t.Fatalf("combined = (%+v, %v)", resp, err)
	}
	// Past-the-end page: empty rows, true total via the COUNT fallback.
	resp, err = client.ListPurchaseOrders(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{Page: 9, PageSize: 2})
	if err != nil || len(resp.GetPurchaseOrders()) != 0 || resp.GetTotal() != 3 {
		t.Fatalf("past-the-end = (%+v, %v)", resp, err)
	}
	// Cross-tenant listing: tenant B sees only its own row.
	resp, err = client.ListPurchaseOrders(callerCtx(gTenantB, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{})
	if err != nil || resp.GetTotal() != 1 || resp.GetPurchaseOrders()[0].GetTenantId() != gTenantB {
		t.Fatalf("tenantB sees (%+v, %v)", resp, err)
	}

	// Validation: unknown status and absurd page answer the #38 machine
	// codes as InvalidArgument.
	_, err = client.ListPurchaseOrders(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{Status: "paused"})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "po_status_invalid" {
		t.Fatalf("bad status: got (%v, %q), want InvalidArgument po_status_invalid", status.Code(err), status.Convert(err).Message())
	}
	_, err = client.ListPurchaseOrders(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.ListPurchaseOrdersRequest{Page: 2000000})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "po_page_invalid" {
		t.Fatalf("bad page: got (%v, %q), want InvalidArgument po_page_invalid", status.Code(err), status.Convert(err).Message())
	}

	// The detail read owns the bodies: items with their ids (#37 exposed
	// them for receipt lines), read by another same-tenant role.
	got, err := client.GetPurchaseOrder(callerCtx(gTenantA, rbac.RoleFinance),
		&procurementv1.GetPurchaseOrderRequest{Id: second.GetId()})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetId() != second.GetId() || got.GetStatus() != "draft" || got.GetTotalCents() != 15450000 {
		t.Fatalf("detail = %+v", got)
	}
	if len(got.GetItems()) != 2 {
		t.Fatalf("items not loaded: %+v", got.GetItems())
	}
	for _, it := range got.GetItems() {
		if it.GetId() == "" {
			t.Fatalf("item id missing: %+v", it)
		}
	}

	// Cross-tenant read is indistinguishable from missing; non-UUID fails
	// fast; metadata-less calls are rejected by the interceptor's re-check
	// (the methods must stay in methodPermissions).
	if _, err := client.GetPurchaseOrder(callerCtx(gTenantB, rbac.RoleRequester),
		&procurementv1.GetPurchaseOrderRequest{Id: second.GetId()}); status.Code(err) != codes.NotFound ||
		status.Convert(err).Message() != "po_not_found" {
		t.Fatalf("cross-tenant get: (%v, %q), want NotFound po_not_found", status.Code(err), status.Convert(err).Message())
	}
	if _, err := client.GetPurchaseOrder(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.GetPurchaseOrderRequest{Id: "not-a-uuid"}); status.Code(err) != codes.InvalidArgument ||
		status.Convert(err).Message() != "po_id_invalid" {
		t.Fatalf("bad id: (%v, %q), want InvalidArgument po_id_invalid", status.Code(err), status.Convert(err).Message())
	}
	if _, err := client.GetPurchaseOrder(context.Background(),
		&procurementv1.GetPurchaseOrderRequest{Id: second.GetId()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous get: code = %v, want Unauthenticated", status.Code(err))
	}
	if _, err := client.ListPurchaseOrders(context.Background(),
		&procurementv1.ListPurchaseOrdersRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous list: code = %v, want Unauthenticated", status.Code(err))
	}
}

// grpcDashboardVendorCode namespaces the dashboard test's vendors so its
// cleanup can target them exactly.
const grpcDashboardVendorCode = "DASH-52-GRPC"

// TestDashboardGRPC covers the #52 summary through the full gRPC wiring:
// the broad dashboard:read at the interceptor, the envelope's three widgets
// against the real domain flow, cross-tenant emptiness, and the
// Unauthenticated/PermissionDenied gates.
func TestDashboardGRPC(t *testing.T) {
	client, db, cleanup := startProcurement(t)
	defer cleanup()
	grpcCleanupRequisition(t, db)
	grpcCleanupVendor(t, db, grpcDashboardVendorCode)
	t.Cleanup(func() {
		grpcCleanupRequisition(t, db)
		grpcCleanupVendor(t, db, grpcDashboardVendorCode)
	})

	// One vendor, one submitted requisition (2 materialized steps, current =
	// step 1), and one approved+converted+issued PO for the spend window.
	v, err := client.CreateVendor(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreateVendorRequest{Code: grpcDashboardVendorCode, Name: "PT Dashboard"})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	if _, err := client.ReplaceApprovalRules(callerCtx(gTenantA, rbac.RoleAdmin),
		&procurementv1.ReplaceApprovalRulesRequest{Rules: testRules()[:1]}); err != nil {
		t.Fatalf("configure rules: %v", err)
	}
	pending, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create pending requisition: %v", err)
	}
	if _, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: pending.GetId()}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	convertible, err := client.CreateRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.CreateRequisitionRequest{Currency: "IDR", Items: testItems()})
	if err != nil {
		t.Fatalf("create convertible requisition: %v", err)
	}
	if _, err := client.SubmitRequisition(callerCtx(gTenantA, rbac.RoleRequester),
		&procurementv1.SubmitRequisitionRequest{Id: convertible.GetId()}); err != nil {
		t.Fatalf("submit convertible: %v", err)
	}
	steps, err := client.GetRequisition(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.GetRequisitionRequest{Id: convertible.GetId()})
	if err != nil {
		t.Fatalf("get requisition: %v", err)
	}
	if _, err := client.DecideApproval(callerCtxAs(gUserB, gTenantA, rbac.RoleApprover),
		&procurementv1.DecideApprovalRequest{StepId: steps.GetApprovalSteps()[0].GetId(), Decision: "approve"}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	po, err := client.CreatePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.CreatePurchaseOrderRequest{RequisitionId: convertible.GetId(), VendorId: v.GetId()})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, err := client.IssuePurchaseOrder(callerCtx(gTenantA, rbac.RoleBuyer),
		&procurementv1.IssuePurchaseOrderRequest{Id: po.GetPurchaseOrder().GetId()}); err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A viewer holds dashboard:read (the broad landing-page read).
	summary, err := client.GetDashboard(callerCtx(gTenantA, rbac.RoleViewer),
		&procurementv1.GetDashboardRequest{})
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}

	// Approvals: total 1 (the one submitted requisition's current step),
	// first page carrying the requisition header and step identity.
	if summary.GetPendingApprovals().GetTotal() != 1 || len(summary.GetPendingApprovals().GetItems()) != 1 {
		t.Fatalf("pending = %+v, want total 1 with the first page", summary.GetPendingApprovals())
	}
	item := summary.GetPendingApprovals().GetItems()[0]
	if item.GetRequisitionId() != pending.GetId() || item.GetNumber() != pending.GetNumber() ||
		item.GetStepNo() != 1 || item.GetRoleRequired() != "approver" || item.GetStepId() == "" ||
		item.GetSubmittedAt() == "" || item.GetTotalCents() != pending.GetTotalCents() {
		t.Fatalf("pending item = %+v, want the submitted requisition's current step with its header", item)
	}

	// Spend: the issued PO (now) is inside both windows.
	if len(summary.GetSpendByVendor()) != 1 {
		t.Fatalf("spend rows = %+v, want the one vendor with issued spend", summary.GetSpendByVendor())
	}
	row := summary.GetSpendByVendor()[0]
	if row.GetVendorId() != v.GetId() || row.GetVendorName() != v.GetName() ||
		row.GetCurrency() != "IDR" || row.GetThirtyDayCents() != po.GetPurchaseOrder().GetTotalCents() ||
		row.GetNinetyDayCents() != po.GetPurchaseOrder().GetTotalCents() {
		t.Fatalf("spend row = %+v, want the issued PO in both windows", row)
	}
	if len(summary.GetSpendTotals()) != 1 ||
		summary.GetSpendTotals()[0].GetThirtyDayCents() != po.GetPurchaseOrder().GetTotalCents() {
		t.Fatalf("spend totals = %+v, want the IDR unbounded sums", summary.GetSpendTotals())
	}

	// Distribution: all seven statuses, one issued PO, zeros elsewhere.
	if len(summary.GetPoStatusDistribution()) != 7 {
		t.Fatalf("distribution = %d rows, want all 7 statuses", len(summary.GetPoStatusDistribution()))
	}
	distSum, issued := int32(0), int32(0)
	for _, d := range summary.GetPoStatusDistribution() {
		if d.GetStatus() == "issued" {
			issued = d.GetCount()
		}
		distSum += d.GetCount()
	}
	if issued != 1 || distSum != 1 {
		t.Fatalf("distribution: issued=%d sum=%d, want 1/1", issued, distSum)
	}

	// Cross-tenant: tenant B's summary is an empty snapshot, not a leak.
	other, err := client.GetDashboard(callerCtx(gTenantB, rbac.RoleViewer),
		&procurementv1.GetDashboardRequest{})
	if err != nil {
		t.Fatalf("get dashboard tenant B: %v", err)
	}
	if other.GetPendingApprovals().GetTotal() != 0 || len(other.GetSpendByVendor()) != 0 {
		t.Fatalf("cross-tenant summary = %+v, want empty", other)
	}

	// Anonymous: the interceptor's map entry rejects metadata-less calls.
	if _, err := client.GetDashboard(context.Background(),
		&procurementv1.GetDashboardRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: code = %v, want Unauthenticated", status.Code(err))
	}
	// A role that holds nothing (an unknown role grants no permission; every
	// catalog role holds the broad dashboard:read) is denied by the
	// interceptor's re-check.
	noRead := metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs(rbac.MDTenantID, gTenantA, rbac.MDUserID, gUser, rbac.MDRole, "ghost"))
	if _, err := client.GetDashboard(noRead,
		&procurementv1.GetDashboardRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown role: code = %v, want PermissionDenied", status.Code(err))
	}
}
