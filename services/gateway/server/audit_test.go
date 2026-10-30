package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// auditTestApp wires the audit routes against fakes and returns a viewer
// header (viewer holds audit:read, the least-privileged holder) plus a
// signer for tokens with other roles.
func auditTestApp(t *testing.T) (*fiber.App, *fakeDocumentsClient, http.Header, func(roles ...string) string) {
	t.Helper()
	documents := &fakeDocumentsClient{}
	app, verifier, priv, kid := newTestApp(t)
	registerAuditRoutes(app, verifier, documents)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		tokenStr, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-44", TenantID: "tenant-44", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return tokenStr
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleViewer))
	return app, documents, header, sign
}

// TestAuditListRoute: viewer lists the tenant's audit rows — the envelope
// carries the bounded entry shape (actor_ip included), the filters forward
// trimmed, and the tenant rides as gRPC metadata.
func TestAuditListRoute(t *testing.T) {
	app, documents, header, _ := auditTestApp(t)
	documents.listAuditResp = &documentsv1.ListAuditResponse{
		Entries: []*documentsv1.AuditEntry{
			{
				EventId: "e-1", TenantId: "tenant-44", ActorId: "user-9",
				ActorIp: "203.0.113.7", Action: "approve",
				EntityType: "requisition", EntityId: "r-1", TraceId: "trace-44",
				OccurredAt: "2026-10-30T08:00:00Z", RecordedAt: "2026-10-30T08:00:01Z",
			},
		},
		NextCursor: "cur-2",
	}

	resp := doReq(t, app, header, http.MethodGet,
		"/api/v1/audit?action=%20approve%20&entityType=requisition&entityId=r-1&limit=50&cursor=cur-1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Entries    []map[string]any `json:"entries"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if len(out.Entries) != 1 || out.Entries[0]["actor_ip"] != "203.0.113.7" ||
		out.Entries[0]["action"] != "approve" || out.NextCursor != "cur-2" {
		t.Fatalf("entries = %s", body)
	}
	if documents.listAuditReq.GetAction() != "approve" ||
		documents.listAuditReq.GetEntityType() != "requisition" ||
		documents.listAuditReq.GetEntityId() != "r-1" ||
		documents.listAuditReq.GetLimit() != 50 ||
		documents.listAuditReq.GetNextCursor() != "cur-1" {
		t.Fatalf("filters forwarded = %+v", documents.listAuditReq)
	}
	md, ok := metadata.FromOutgoingContext(documents.lastCtx)
	if !ok || md.Get("x-tenant-id")[0] != "tenant-44" {
		t.Fatalf("tenant metadata missing: %v", md)
	}
}

// TestAuditRouteRequiresPermission: audit:read is viewer/admin — a buyer is
// refused at the edge with the machine-readable reason, before any
// documents call; anonymous is 401.
func TestAuditRouteRequiresPermission(t *testing.T) {
	app, documents, header, sign := auditTestApp(t)
	buyerHeader := header.Clone()
	buyerHeader.Set("Authorization", "Bearer "+sign(rbac.RoleBuyer))

	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		resp := doReq(t, app, buyerHeader, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as buyer: status = %d, want 403", path, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		var out struct {
			Code     string `json:"code"`
			Required string `json:"required"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		if out.Code != "permission_denied" || out.Required != "audit:read" {
			t.Fatalf("body = %s", body)
		}
	}
	if documents.lastCtx != nil {
		t.Fatal("documents must not be called for a denied role")
	}

	resp := doReq(t, app, http.Header{}, http.MethodGet, "/api/v1/audit", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: status = %d, want 401", resp.StatusCode)
	}
}

// TestAuditExportRoute: the export streams documents' CSV bytes as a
// text/csv attachment; documents' machine codes ride through (400 for an
// invalid action, 502 shape for an unreachable service).
func TestAuditExportRoute(t *testing.T) {
	app, documents, header, _ := auditTestApp(t)
	documents.exportAuditResp = &documentsv1.ExportAuditResponse{
		Csv: []byte("event_id,occurred_at\r\ne-1,2026-10-30T08:00:00Z\r\n"),
	}

	resp := doReq(t, app, header, http.MethodGet, "/api/v1/audit/export", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "audit-log.csv") {
		t.Fatalf("content disposition = %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "event_id,occurred_at") {
		t.Fatalf("csv body = %q", body)
	}

	documents.exportAuditResp = nil
	documents.exportAuditErr = status.Error(codes.InvalidArgument, "audit_action_invalid")
	resp = doReq(t, app, header, http.MethodGet, "/api/v1/audit/export", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid action status = %d, want 400", resp.StatusCode)
	}
}

// TestForwardContextActorIP pins both branches of the trusted-proxy check
// (#44): with no trusted proxies an X-Forwarded-For header is ignored and
// the socket peer's address is what flows as x-actor-ip; with the peer
// trusted, the forwarded value wins. The audit trail must not be spoofable
// by a direct client.
func TestForwardContextActorIP(t *testing.T) {
	makeApp := func(trusted []string) (*fiber.App, *fakeDocumentsClient, http.Header) {
		pub, priv, _ := jwt.GenerateKey()
		kid := jwt.Kid(pub)
		verifier := token.NewVerifier(&staticFetcher{keys: []token.Key{{Kid: kid, Ed25519: pub, Current: true}}}, time.Hour)
		log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
		documents := &fakeDocumentsClient{}
		documents.listAuditResp = &documentsv1.ListAuditResponse{}
		opts := []Option{WithTrustedProxies(trusted)}
		if trusted == nil {
			opts = nil // the default: trust no proxy
		}
		app := New(log, verifier, opts...)
		registerAuditRoutes(app, verifier, documents)
		t.Cleanup(func() { _ = app.Shutdown() })

		now := time.Now()
		tokenStr, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-44", TenantID: "tenant-44", Roles: []string{rbac.RoleViewer},
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		header := http.Header{}
		header.Set("Authorization", "Bearer "+tokenStr)
		header.Set("X-Forwarded-For", "203.0.113.9")
		return app, documents, header
	}

	// Branch 1 — no trusted proxies: the spoofed header is ignored and the
	// socket peer (0.0.0.0 under app.Test) is recorded.
	app, documents, header := makeApp(nil)
	if resp := doReq(t, app, header, http.MethodGet, "/api/v1/audit", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("untrusted list status = %d", resp.StatusCode)
	}
	md, ok := metadata.FromOutgoingContext(documents.lastCtx)
	if !ok {
		t.Fatal("documents not called")
	}
	if got := md.Get("x-actor-ip"); len(got) != 1 || got[0] != "0.0.0.0" {
		t.Fatalf("x-actor-ip = %v, want the socket peer 0.0.0.0", got)
	}

	// Branch 2 — the peer trusted: the forwarded value is honored.
	app, documents, header = makeApp([]string{"0.0.0.0/0"})
	if resp := doReq(t, app, header, http.MethodGet, "/api/v1/audit", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("trusted list status = %d", resp.StatusCode)
	}
	md, ok = metadata.FromOutgoingContext(documents.lastCtx)
	if !ok {
		t.Fatal("documents not called")
	}
	if got := md.Get("x-actor-ip"); len(got) != 1 || got[0] != "203.0.113.9" {
		t.Fatalf("x-actor-ip = %v, want the forwarded 203.0.113.9", got)
	}
}
