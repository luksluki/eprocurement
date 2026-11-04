package server

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/dlq"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// wrapErr mimics how the real client wraps its typed errors (fmt.Errorf
// with %w), which is what the route's errors.Is mapping relies on.
func wrapErr(err error) error { return fmt.Errorf("dlq: session: %w", err) }

// fakeDLQAdmin records what the routes forwarded to the broker client.
type fakeDLQAdmin struct {
	listErr  error
	listRes  dlq.ListResult
	gotList  bool
	listTeno string // tenant forwarded to List
	listLim  int    // limit forwarded to List

	replayErr    error
	gotReplay    bool
	replayTenant string
	replayID     string
}

func (f *fakeDLQAdmin) List(ctx context.Context, tenantID string, limit int) (dlq.ListResult, error) {
	f.gotList, f.listTeno, f.listLim = true, tenantID, limit
	if f.listErr != nil {
		return dlq.ListResult{}, f.listErr
	}
	return f.listRes, nil
}

func (f *fakeDLQAdmin) Replay(ctx context.Context, tenantID, messageID string) error {
	f.gotReplay, f.replayTenant, f.replayID = true, tenantID, messageID
	return f.replayErr
}

// dlqApp wires the DLQ routes against fake on a fresh key set, returning
// the app with its signer material (each app verifies only its own keys).
func dlqApp(t *testing.T, fake *fakeDLQAdmin) (*fiber.App, ed25519.PrivateKey, string) {
	t.Helper()
	app, verifier, priv, kid := newTestApp(t)
	registerDLQRoutes(app, verifier, fake)
	return app, priv, kid
}

func dlqSign(t *testing.T, priv ed25519.PrivateKey, kid, tenant string, roles ...string) string {
	t.Helper()
	now := time.Now()
	token, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "user-48", TenantID: tenant, Roles: roles,
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

// dlqTestApp wires the DLQ routes against the fake and returns an admin
// token plus a signer for tokens of other roles/tenants.
func dlqTestApp(t *testing.T) (*fiber.App, *fakeDLQAdmin, string, func(tenant string, roles ...string) string) {
	t.Helper()
	fake := &fakeDLQAdmin{}
	app, priv, kid := dlqApp(t, fake)
	sign := func(tenant string, roles ...string) string {
		return dlqSign(t, priv, kid, tenant, roles...)
	}
	return app, fake, sign("tenant-48", rbac.RoleAdmin), sign
}

func sampleDLQMessages() []dlq.Message {
	died := time.Date(2026, 10, 28, 10, 5, 0, 0, time.UTC)
	return []dlq.Message{
		{
			MessageID: "evt-1", EventID: "evt-1", TenantID: "tenant-48", Type: "pr.submitted",
			OccurredAt: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC), RetryCount: 3,
			OriginalQueue: "documents.history", Reason: "rejected", DiedAt: died,
		},
		{
			MessageID: "evt-broken", DecodeError: true, RetryCount: 0,
			OriginalQueue: "notifications", Reason: "rejected", DiedAt: died,
		},
	}
}

// AC (#48): the DLQ routes are admin-only (dlq:manage — every non-admin
// role is denied 403 at the edge BEFORE the broker is touched), and a
// missing/invalid token is 401.
func TestDLQRoutesRBAC(t *testing.T) {
	for name, role := range map[string]string{
		"requester": rbac.RoleRequester,
		"approver":  rbac.RoleApprover,
		"buyer":     rbac.RoleBuyer,
		"finance":   rbac.RoleFinance,
		"viewer":    rbac.RoleViewer,
	} {
		app, fake, _, sign := dlqTestApp(t)
		resp, body := reqDo(t, app, "GET", "/api/v1/dlq", sign("tenant-48", role), "", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s list: status = %d, body = %v", name, resp.StatusCode, body)
		}
		if body["required"] != rbac.DLQManage {
			t.Fatalf("%s list: body = %v, want required %s", name, body, rbac.DLQManage)
		}
		if fake.gotList {
			t.Fatalf("%s list: broker client called despite the denial", name)
		}
		resp, body = reqDo(t, app, "POST", "/api/v1/dlq/replay", sign("tenant-48", role), `{"message_id":"evt-1"}`, nil)
		if resp.StatusCode != http.StatusForbidden || fake.gotReplay {
			t.Fatalf("%s replay: status = %d, called = %v", name, resp.StatusCode, fake.gotReplay)
		}
	}

	// Missing / garbage token: 401 before anything else.
	app, fake, _, _ := dlqTestApp(t)
	for name, header := range map[string]string{"no token": "", "garbage": "Bearer garbage"} {
		resp, _ := reqDo(t, app, "GET", "/api/v1/dlq", header, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
	if fake.gotList || fake.gotReplay {
		t.Fatal("broker client called without a verified token")
	}
}

// AC (#48): the admin list shows the tenant's decoded entries plus
// undecodable entries as metadata only (no tenant, no business fields),
// with the {depth, scanned, messages} envelope; the tenant comes from the
// claims, and limit defaults to 50 / caps at 500.
func TestDLQList(t *testing.T) {
	app, fake, adminToken, _ := dlqTestApp(t)
	fake.listRes = dlq.ListResult{Depth: 7, Scanned: 7, Messages: sampleDLQMessages()}

	resp, body := reqDo(t, app, "GET", "/api/v1/dlq", adminToken, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status = %d, body = %v", resp.StatusCode, body)
	}
	if body["depth"] != float64(7) || body["scanned"] != float64(7) {
		t.Fatalf("list envelope = %v", body)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", body["messages"])
	}
	decoded := msgs[0].(map[string]any)
	if decoded["event_id"] != "evt-1" || decoded["tenant_id"] != "tenant-48" ||
		decoded["type"] != "pr.submitted" || decoded["decode_error"] != false ||
		decoded["retry_count"] != float64(3) || decoded["original_queue"] != "documents.history" ||
		decoded["reason"] != "rejected" {
		t.Fatalf("decoded entry = %v", decoded)
	}
	if _, has := decoded["occurred_at"]; !has {
		t.Fatalf("decoded entry missing occurred_at: %v", decoded)
	}
	broken := msgs[1].(map[string]any)
	if broken["decode_error"] != true || broken["message_id"] != "evt-broken" {
		t.Fatalf("decode_error entry = %v", broken)
	}
	for _, leaked := range []string{"tenant_id", "event_id", "type", "occurred_at"} {
		if _, has := broken[leaked]; has {
			t.Fatalf("decode_error entry leaked %q: %v", leaked, broken)
		}
	}
	if fake.listTeno != "tenant-48" {
		t.Fatalf("tenant forwarded to List = %q, want the claims' tenant-48", fake.listTeno)
	}
	if fake.listLim != 50 {
		t.Fatalf("default limit forwarded = %d, want 50", fake.listLim)
	}

	// limit parsing: explicit value passes through, over-max caps at 500,
	// garbage falls back to the default.
	for name, tc := range map[string]struct {
		query string
		want  int
	}{
		"explicit": {"?limit=10", 10},
		"over max": {"?limit=9999", 500},
		"garbage":  {"?limit=abc", 50},
		"negative": {"?limit=-5", 50},
		"zero":     {"?limit=0", 50},
	} {
		fake2 := &fakeDLQAdmin{}
		app2, priv2, kid2 := dlqApp(t, fake2)
		resp2, _ := reqDo(t, app2, "GET", "/api/v1/dlq"+tc.query,
			dlqSign(t, priv2, kid2, "tenant-48", rbac.RoleAdmin), "", nil)
		if resp2.StatusCode != http.StatusOK || fake2.listLim != tc.want {
			t.Fatalf("%s: status = %d, forwarded limit = %d, want %d", name, resp2.StatusCode, fake2.listLim, tc.want)
		}
	}
}

// AC (#48): replay forwards message_id and the claims' tenant; the typed
// client errors map to 404 (cross-tenant/unknown), 409 (undecodable), 503
// (broker down), and bad input is 400 before the broker is touched.
func TestDLQReplay(t *testing.T) {
	app, fake, token, _ := dlqTestApp(t)

	resp, body := reqDo(t, app, "POST", "/api/v1/dlq/replay", token, `{"message_id":"evt-1"}`, nil)
	if resp.StatusCode != http.StatusOK || body["replayed"] != "evt-1" {
		t.Fatalf("replay: status = %d, body = %v", resp.StatusCode, body)
	}
	if fake.replayTenant != "tenant-48" || fake.replayID != "evt-1" {
		t.Fatalf("replay forwarded tenant/id = %q/%q", fake.replayTenant, fake.replayID)
	}

	for name, tc := range map[string]struct {
		err  error
		code int
		want string
	}{
		"cross-tenant or unknown": {wrapErr(dlq.ErrMessageNotFound), http.StatusNotFound, "dlq_message_not_found"},
		"undecodable":             {wrapErr(dlq.ErrNotReplayable), http.StatusConflict, "dlq_message_not_replayable"},
		"broker down":             {wrapErr(dlq.ErrBrokerUnavailable), http.StatusServiceUnavailable, "dlq_broker_unavailable"},
	} {
		fake2 := &fakeDLQAdmin{replayErr: tc.err}
		app2, priv2, kid2 := dlqApp(t, fake2)
		resp2, body2 := reqDo(t, app2, "POST", "/api/v1/dlq/replay",
			dlqSign(t, priv2, kid2, "tenant-48", rbac.RoleAdmin), `{"message_id":"evt-1"}`, nil)
		if resp2.StatusCode != tc.code || body2["code"] != tc.want {
			t.Fatalf("%s: status = %d, body = %v, want %d/%s", name, resp2.StatusCode, body2, tc.code, tc.want)
		}
	}

	// Input validation: malformed body and empty message_id are 400 with
	// no broker call.
	for name, tc := range map[string]struct {
		body string
		code string
	}{
		"malformed body":   {"{not json", "body_invalid"},
		"empty message_id": {`{"message_id":"  "}`, "dlq_message_id_invalid"},
	} {
		fake2 := &fakeDLQAdmin{}
		app2, priv2, kid2 := dlqApp(t, fake2)
		resp2, body2 := reqDo(t, app2, "POST", "/api/v1/dlq/replay",
			dlqSign(t, priv2, kid2, "tenant-48", rbac.RoleAdmin), tc.body, nil)
		if resp2.StatusCode != http.StatusBadRequest || body2["code"] != tc.code || fake2.gotReplay {
			t.Fatalf("%s: status = %d, body = %v, called = %v", name, resp2.StatusCode, body2, fake2.gotReplay)
		}
	}

	// The list maps the broker error the same way.
	fake3 := &fakeDLQAdmin{listErr: wrapErr(dlq.ErrBrokerUnavailable)}
	app3, priv3, kid3 := dlqApp(t, fake3)
	resp3, body3 := reqDo(t, app3, "GET", "/api/v1/dlq",
		dlqSign(t, priv3, kid3, "tenant-48", rbac.RoleAdmin), "", nil)
	if resp3.StatusCode != http.StatusServiceUnavailable || body3["code"] != "dlq_broker_unavailable" {
		t.Fatalf("list broker error: status = %d, body = %v", resp3.StatusCode, body3)
	}
}
