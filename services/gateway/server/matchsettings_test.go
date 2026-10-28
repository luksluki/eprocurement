package server

import (
	"context"
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

// fakeMatchSettingsClient satisfies ProcurementClient (vendor +
// requisition via the embedded fakes) and records what the gateway
// forwarded to the settings calls.
type fakeMatchSettingsClient struct {
	*fakeRequisitionClient

	getRes  *procurementv1.MatchSettings
	getErr  error
	getCtx  context.Context
	getSeen bool

	replaceReq *procurementv1.ReplaceMatchSettingsRequest
	replaceRes *procurementv1.MatchSettings
	replaceErr error
	replaceCtx context.Context
}

func (f *fakeMatchSettingsClient) MatchInvoice(ctx context.Context, in *procurementv1.MatchInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error) {
	return nil, status.Error(codes.Unimplemented, "not called in settings tests")
}

func (f *fakeMatchSettingsClient) GetMatchSettings(ctx context.Context, in *procurementv1.GetMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error) {
	f.getCtx, f.getSeen = ctx, true
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getRes, nil
}

func (f *fakeMatchSettingsClient) ReplaceMatchSettings(ctx context.Context, in *procurementv1.ReplaceMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error) {
	f.replaceCtx, f.replaceReq = ctx, in
	if f.replaceErr != nil {
		return nil, f.replaceErr
	}
	return f.replaceRes, nil
}

// matchSettingsTestApp wires the settings routes against fakes and returns
// an admin header plus a signer for tokens with other roles.
func matchSettingsTestApp(t *testing.T) (*fiber.App, *fakeMatchSettingsClient, http.Header, func(roles ...string) string) {
	t.Helper()
	fake := &fakeMatchSettingsClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		getRes:                &procurementv1.MatchSettings{QtyPct: 0, PricePct: 0},
		replaceRes:            &procurementv1.MatchSettings{QtyPct: 5, PricePct: 2},
	}
	app, verifier, priv, kid := newTestApp(t)
	registerMatchSettingsRoutes(app, verifier, fake)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-40", TenantID: "tenant-40", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleAdmin))
	header.Set("Content-Type", "application/json")
	return app, fake, header, sign
}

// AC (#40): GET|PUT /api/v1/match-settings are admin-only (match:configure
// — the approval:configure twin). Finance, buyer, and viewer are denied
// 403 at the edge before any procurement call; the admin's calls forward
// the body and the claims as gRPC metadata.
func TestMatchSettingsRoutes(t *testing.T) {
	for name, role := range map[string]string{
		"finance": rbac.RoleFinance,
		"buyer":   rbac.RoleBuyer,
		"viewer":  rbac.RoleViewer,
	} {
		app, fake, _, sign := matchSettingsTestApp(t)
		resp, body := reqDo(t, app, "GET", "/api/v1/match-settings", sign(role), "", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s get: status = %d, body = %v", name, resp.StatusCode, body)
		}
		if body["required"] != rbac.MatchConfigure {
			t.Fatalf("%s get: body = %v, want required %s", name, body, rbac.MatchConfigure)
		}
		if fake.getSeen {
			t.Fatalf("%s get: procurement called despite the denial", name)
		}
		resp, body = reqDo(t, app, "PUT", "/api/v1/match-settings", sign(role), `{"qty_pct":5,"price_pct":2}`, nil)
		if resp.StatusCode != http.StatusForbidden || fake.replaceReq != nil {
			t.Fatalf("%s put: status = %d, called = %v", name, resp.StatusCode, fake.replaceReq)
		}
	}

	// Admin GET: the effective defaults while nothing is stored.
	app, fake, header, _ := matchSettingsTestApp(t)
	resp, body := reqDo(t, app, "GET", "/api/v1/match-settings", header.Get("Authorization")[7:], "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get: status = %d, body = %v", resp.StatusCode, body)
	}
	if body["qty_pct"] != float64(0) || body["price_pct"] != float64(0) {
		t.Fatalf("admin get body = %v, want the 0/0 defaults", body)
	}
	md := metadataOf(t, fake.getCtx)
	if md["x-tenant-id"] != "tenant-40" || md["x-user-id"] != "user-40" || md["x-role"] != rbac.RoleAdmin {
		t.Fatalf("propagated metadata = %v", md)
	}

	// Admin PUT: the body is forwarded verbatim, the stored values echo.
	resp, body = reqDo(t, app, "PUT", "/api/v1/match-settings", header.Get("Authorization")[7:], `{"qty_pct":5,"price_pct":2}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin put: status = %d, body = %v", resp.StatusCode, body)
	}
	if body["qty_pct"] != float64(5) || body["price_pct"] != float64(2) {
		t.Fatalf("admin put body = %v", body)
	}
	if fake.replaceReq.GetQtyPct() != 5 || fake.replaceReq.GetPricePct() != 2 {
		t.Fatalf("forwarded request = %+v", fake.replaceReq)
	}
}

// AC (#40): the settings routes' error mapping — the 0..100 bound codes
// render 400 through the shared mapping, and a malformed body is a 400
// before any call.
func TestMatchSettingsRouteErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		want string
	}{
		"qty pct invalid":   {status.Error(codes.InvalidArgument, "match_qty_pct_invalid"), http.StatusBadRequest, "match_qty_pct_invalid"},
		"price pct invalid": {status.Error(codes.InvalidArgument, "match_price_pct_invalid"), http.StatusBadRequest, "match_price_pct_invalid"},
	} {
		app, fake, header, _ := matchSettingsTestApp(t)
		fake.replaceErr = tc.err
		resp, body := reqDo(t, app, "PUT", "/api/v1/match-settings", header.Get("Authorization")[7:], `{"qty_pct":1}`, nil)
		if resp.StatusCode != tc.code {
			t.Fatalf("%s: status = %d, body = %v", name, resp.StatusCode, body)
		}
		if body["code"] != tc.want {
			t.Fatalf("%s: code = %v, want %q", name, body["code"], tc.want)
		}
	}
	app, _, header, _ := matchSettingsTestApp(t)
	resp, body := reqDo(t, app, "PUT", "/api/v1/match-settings", header.Get("Authorization")[7:], `{not json`, nil)
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "body_invalid" {
		t.Fatalf("malformed body: status = %d, body = %v", resp.StatusCode, body)
	}
}
