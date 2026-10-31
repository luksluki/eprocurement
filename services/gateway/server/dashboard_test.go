package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// fakeDashboardClient satisfies ProcurementClient (vendor + requisition via
// the embedded fakes) and records what the gateway forwarded to the summary
// call.
type fakeDashboardClient struct {
	*fakeRequisitionClient

	dashReq *procurementv1.GetDashboardRequest
	dashRes *procurementv1.DashboardSummary
	dashErr error
	dashCtx context.Context
}

func (f *fakeDashboardClient) GetDashboard(ctx context.Context, in *procurementv1.GetDashboardRequest, opts ...grpc.CallOption) (*procurementv1.DashboardSummary, error) {
	f.dashCtx, f.dashReq = ctx, in
	if f.dashErr != nil {
		return nil, f.dashErr
	}
	return f.dashRes, nil
}

// sampleDashboard builds one fully populated summary row set.
func sampleDashboard() *procurementv1.DashboardSummary {
	return &procurementv1.DashboardSummary{
		PendingApprovals: &procurementv1.PendingApprovals{
			Total: 7,
			Items: []*procurementv1.PendingApproval{{
				RequisitionId: "req-1", Number: "PR-000001", Currency: "IDR",
				TotalCents: 1_545_000, SubmittedAt: "2026-10-30T08:00:00Z",
				StepId: "step-1", StepNo: 1, RoleRequired: "approver",
			}},
		},
		SpendByVendor: []*procurementv1.VendorSpend{{
			VendorId: "vendor-1", VendorName: "PT Spend", Currency: "IDR",
			ThirtyDayCents: 1_000_000, NinetyDayCents: 3_000_000,
		}},
		PoStatusDistribution: []*procurementv1.PurchaseOrderStatusCount{
			{Status: "draft", Count: 0}, {Status: "issued", Count: 2},
			{Status: "acknowledged", Count: 0}, {Status: "partially_received", Count: 0},
			{Status: "received", Count: 1}, {Status: "closed", Count: 0},
			{Status: "cancelled", Count: 0},
		},
		SpendTotals: []*procurementv1.CurrencySpend{{
			Currency: "IDR", ThirtyDayCents: 1_000_000, NinetyDayCents: 3_000_000,
		}},
	}
}

// dashboardTestApp wires the dashboard route against a fake and returns a
// viewer header (dashboard:read is broad) plus a signer for other roles.
func dashboardTestApp(t *testing.T) (*fiber.App, *fakeDashboardClient, http.Header, func(roles ...string) string) {
	t.Helper()
	fake := &fakeDashboardClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		dashRes:               sampleDashboard(),
	}
	app, verifier, priv, kid := newTestApp(t)
	registerDashboardRoutes(app, verifier, fake)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-52", TenantID: "tenant-52", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleViewer))
	return app, fake, header, sign
}

// AC (#52): the summary route is wired through server.New with
// WithProcurement, the broad dashboard:read (a viewer passes), the response
// carries all three widgets plus the per-currency totals, and the claims
// ride along as gRPC metadata.
func TestDashboardRouteThroughServerNew(t *testing.T) {
	fake := &fakeDashboardClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		dashRes:               sampleDashboard(),
	}
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })
	now := time.Now()
	token, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "user-52", TenantID: "tenant-52", Roles: []string{rbac.RoleViewer},
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	resp, body := reqDo(t, app, "GET", "/api/v1/dashboard/summary", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("summary status = %d, body = %v", resp.StatusCode, body)
	}
	pending, _ := body["pending_approvals"].(map[string]any)
	if pending["total"] != float64(7) {
		t.Fatalf("pending = %v, want total 7", pending)
	}
	items, _ := pending["items"].([]any)
	row, _ := items[0].(map[string]any)
	if row["requisition_id"] != "req-1" || row["step_no"] != float64(1) || row["role_required"] != "approver" ||
		row["total_cents"] != float64(1_545_000) || row["submitted_at"] != "2026-10-30T08:00:00Z" {
		t.Fatalf("pending row = %v", row)
	}
	spend, _ := body["spend_by_vendor"].([]any)
	spendRow, _ := spend[0].(map[string]any)
	if spendRow["vendor_name"] != "PT Spend" || spendRow["thirty_day_cents"] != float64(1_000_000) ||
		spendRow["ninety_day_cents"] != float64(3_000_000) {
		t.Fatalf("spend row = %v", spendRow)
	}
	dist, _ := body["po_status_distribution"].([]any)
	if len(dist) != 7 {
		t.Fatalf("distribution = %v, want all 7 statuses", dist)
	}
	distRow, _ := dist[0].(map[string]any)
	if distRow["status"] != "draft" || distRow["count"] != float64(0) {
		t.Fatalf("distribution row = %v, want the zero draft filler first", distRow)
	}
	totals, _ := body["spend_totals"].([]any)
	totalRow, _ := totals[0].(map[string]any)
	if totalRow["currency"] != "IDR" || totalRow["ninety_day_cents"] != float64(3_000_000) {
		t.Fatalf("spend totals = %v", totals)
	}
	// The empty request went through with the claims as gRPC metadata.
	if fake.dashReq == nil {
		t.Fatal("procurement was not called")
	}
	md := metadataOf(t, fake.dashCtx)
	if md["x-tenant-id"] != "tenant-52" || md["x-user-id"] != "user-52" {
		t.Fatalf("metadata = %v", md)
	}
}

// AC (#52): the route requires a Bearer token (401 anonymous before any
// procurement call) and a role holding dashboard:read (403 otherwise — an
// unknown role grants nothing); a procurement failure renders through the
// shared mapping (Internal → 502).
func TestDashboardRouteErrors(t *testing.T) {
	app, fake, header, sign := dashboardTestApp(t)
	resp, _ := reqDo(t, app, "GET", "/api/v1/dashboard/summary", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", resp.StatusCode)
	}
	if fake.dashReq != nil {
		t.Fatal("procurement called without a token")
	}

	resp, _ = reqDo(t, app, "GET", "/api/v1/dashboard/summary", sign("ghost"), "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown-role status = %d, want 403", resp.StatusCode)
	}

	fake.dashErr = status.Error(codes.Internal, "internal error")
	resp, _ = reqDo(t, app, "GET", "/api/v1/dashboard/summary", tokenOf(header), "", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("internal status = %d, want 502", resp.StatusCode)
	}
}
