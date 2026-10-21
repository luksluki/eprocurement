package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// fakeVendorClient records the outgoing metadata the gateway propagated and
// replies with canned vendors or errors.
type fakeVendorClient struct {
	lastCtx    context.Context
	lastCalled string
	err        error
	vendor     *procurementv1.Vendor

	listReq *procurementv1.ListVendorsRequest
	listRes *procurementv1.ListVendorsResponse
	listErr error
}

func (f *fakeVendorClient) ListVendors(ctx context.Context, in *procurementv1.ListVendorsRequest, opts ...grpc.CallOption) (*procurementv1.ListVendorsResponse, error) {
	f.lastCtx, f.lastCalled, f.listReq = ctx, "list", in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRes, nil
}

func (f *fakeVendorClient) CreateVendor(ctx context.Context, in *procurementv1.CreateVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error) {
	f.lastCtx, f.lastCalled = ctx, "create"
	return f.reply()
}
func (f *fakeVendorClient) GetVendor(ctx context.Context, in *procurementv1.GetVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error) {
	f.lastCtx, f.lastCalled = ctx, "get"
	return f.reply()
}
func (f *fakeVendorClient) UpdateVendor(ctx context.Context, in *procurementv1.UpdateVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error) {
	f.lastCtx, f.lastCalled = ctx, "update"
	return f.reply()
}
func (f *fakeVendorClient) DeactivateVendor(ctx context.Context, in *procurementv1.DeactivateVendorRequest, opts ...grpc.CallOption) (*procurementv1.Vendor, error) {
	f.lastCtx, f.lastCalled = ctx, "deactivate"
	return f.reply()
}

func (f *fakeVendorClient) reply() (*procurementv1.Vendor, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.vendor, nil
}

func TestVendorRoutesHappyPathAndPropagation(t *testing.T) {
	fake := &fakeVendorClient{vendor: &procurementv1.Vendor{
		Id: "v-1", TenantId: "t-1", Code: "G-01", Name: "PT Contoh", Status: "active",
		CreatedAt: "2026-10-21T10:00:00Z", CreatedBy: "u-1", UpdatedAt: "2026-10-21T10:00:00Z", UpdatedBy: "u-1",
	}}

	app, verifier, priv, kid := newTestApp(t)
	registerVendorRoutes(app, verifier, fake, nil)

	now := time.Now()
	token, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Iss: "identity.eprocurement", Sub: "user-9", TenantID: "tenant-9",
		Roles: []string{rbac.RoleBuyer}, IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/vendors", bytes.NewReader([]byte(`{"code":"g-01","name":"PT Contoh"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}

	// The gateway propagated the verified claims to procurement.
	md, ok := metadata.FromOutgoingContext(fake.lastCtx)
	if !ok {
		t.Fatal("no outgoing metadata on the procurement call")
	}
	if got := md.Get(rbac.MDTenantID); len(got) != 1 || got[0] != "tenant-9" {
		t.Fatalf("tenant metadata = %v", got)
	}
	if got := md.Get(rbac.MDUserID); len(got) != 1 || got[0] != "user-9" {
		t.Fatalf("user metadata = %v", got)
	}
	if got := md.Get(rbac.MDRole); len(got) != 1 || got[0] != rbac.RoleBuyer {
		t.Fatalf("role metadata = %v", got)
	}
}

func TestVendorRoutesRequirePermission(t *testing.T) {
	fake := &fakeVendorClient{vendor: &procurementv1.Vendor{Id: "v-1"}}
	app, verifier, priv, kid := newTestApp(t)
	registerVendorRoutes(app, verifier, fake, nil)

	now := time.Now()
	viewerToken, _ := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "u", TenantID: "t", Roles: []string{rbac.RoleViewer},
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)

	for _, method := range []string{"POST /api/v1/vendors", "PATCH /api/v1/vendors/v-1", "POST /api/v1/vendors/v-1/deactivate"} {
		parts := strings.SplitN(method, " ", 2)
		req := httptest.NewRequest(parts[0], parts[1], nil)
		req.Header.Set("Authorization", "Bearer "+viewerToken)
		resp, _ := app.Test(req, 5000)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as viewer: status = %d, want 403", method, resp.StatusCode)
		}
	}
}

func TestVendorRoutesErrorMapping(t *testing.T) {
	fake := &fakeVendorClient{err: status.Error(codes.AlreadyExists, "vendor_duplicate_code")}
	app, verifier, priv, kid := newTestApp(t)
	registerVendorRoutes(app, verifier, fake, nil)

	now := time.Now()
	token, _ := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "u", TenantID: "t", Roles: []string{rbac.RoleBuyer},
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)

	req := httptest.NewRequest("POST", "/api/v1/vendors", bytes.NewReader([]byte(`{"code":"DUP","name":"PT X"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"vendor_duplicate_code"`) {
		t.Fatalf("body = %s, want machine code preserved", body)
	}
}

func TestVendorListRoute(t *testing.T) {
	fake := &fakeVendorClient{
		listRes: &procurementv1.ListVendorsResponse{
			Vendors: []*procurementv1.Vendor{
				{Id: "v-1", Code: "A-01", Name: "PT Satu", Status: "active"},
				{Id: "v-2", Code: "A-02", Name: "PT Dua", Status: "deactivated"},
			},
			Total: 42, Page: 2, PageSize: 2,
		},
	}
	app, verifier, priv, kid := newTestApp(t)
	registerVendorRoutes(app, verifier, fake, nil)

	now := time.Now()
	token, _ := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "u", TenantID: "t", Roles: []string{rbac.RoleViewer},
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)

	req := httptest.NewRequest("GET", "/api/v1/vendors?q=pt&status=active&page=2&page_size=2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Vendors  []map[string]any `json:"vendors"`
		Total    float64          `json:"total"`
		Page     float64          `json:"page"`
		PageSize float64          `json:"page_size"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if len(out.Vendors) != 2 || out.Total != 42 || out.Page != 2 || out.PageSize != 2 {
		t.Fatalf("body = %s", body)
	}
	if out.Vendors[0]["code"] != "A-01" {
		t.Fatalf("vendor JSON wrong: %s", body)
	}

	// Query params were forwarded to procurement.
	if fake.listReq.GetQuery() != "pt" || fake.listReq.GetStatus() != "active" ||
		fake.listReq.GetPage() != 2 || fake.listReq.GetPageSize() != 2 {
		t.Fatalf("forwarded request = %+v", fake.listReq)
	}
	md, ok := metadata.FromOutgoingContext(fake.lastCtx)
	if !ok || len(md.Get(rbac.MDTenantID)) != 1 || md.Get(rbac.MDTenantID)[0] != "t" {
		t.Fatal("tenant metadata not propagated on list")
	}

	// Invalid status from procurement maps to 400 with the machine code.
	fake.listErr = status.Error(codes.InvalidArgument, "vendor_status_invalid")
	badReq := httptest.NewRequest("GET", "/api/v1/vendors?status=paused", nil)
	badReq.Header.Set("Authorization", "Bearer "+token)
	resp, err = app.Test(badReq, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"vendor_status_invalid"`) {
		t.Fatalf("body = %s", body)
	}
}
