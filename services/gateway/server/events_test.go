package server

import (
	"bytes"
	"log/slog"
	"net/http"
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

// eventsTestApp wires the event routes against a fake documents client and
// returns a buyer header plus a signer for tokens with other roles.
func eventsTestApp(t *testing.T) (*fiber.App, *fakeDocumentsClient, http.Header, func(roles ...string) string) {
	t.Helper()
	documents := &fakeDocumentsClient{}

	app, verifier, priv, kid := newTestApp(t)
	registerEventRoutes(app, verifier, documents)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-9", TenantID: "tenant-9", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleBuyer))
	return app, documents, header, sign
}

func seedEvent(id, eventType, occurredAt string) *documentsv1.Event {
	return &documentsv1.Event{
		EventId:     id,
		TenantId:    "tenant-9",
		Type:        eventType,
		Version:     1,
		OccurredAt:  occurredAt,
		ActorId:     "user-1",
		ActorIp:     "203.0.113.7",
		TraceId:     "trace-" + id,
		EntityType:  "requisition",
		EntityId:    "r-1",
		PayloadJson: `{"status":"approved"}`,
	}
}

func TestListEventsReturnsEnvelopeAndCursor(t *testing.T) {
	app, documents, header, _ := eventsTestApp(t)
	documents.listEventsResp = &documentsv1.ListEventsResponse{
		Events: []*documentsv1.Event{
			seedEvent("ev-2", "pr.approved", "2026-10-26T09:00:00Z"),
			seedEvent("ev-1", "pr.submitted", "2026-10-26T08:00:00Z"),
		},
		NextCursor: "Y3Vyc29y",
	}

	resp, body := do(t, app, http.MethodGet,
		"/api/v1/events?entityType=requisition&entityId=r-1&limit=50&cursor=abc", header.Get("Authorization"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	events, _ := body["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events = %v", body)
	}
	first, _ := events[0].(map[string]any)
	want := map[string]any{
		"event_id": "ev-2", "tenant_id": "tenant-9", "type": "pr.approved",
		"version": float64(1), "occurred_at": "2026-10-26T09:00:00Z",
		"actor_id": "user-1", "actor_ip": "203.0.113.7", "trace_id": "trace-ev-2",
		"entity_type": "requisition", "entity_id": "r-1",
		"payload_json": `{"status":"approved"}`,
	}
	for k, v := range want {
		if first[k] != v {
			t.Fatalf("event[%s] = %v, want %v (body %v)", k, first[k], v, body)
		}
	}
	if body["next_cursor"] != "Y3Vyc29y" {
		t.Fatalf("next_cursor = %v", body["next_cursor"])
	}

	// Query params forwarded verbatim (camelCase per PRD §15) and the tenant
	// from the claims — never from the query — reached documents as metadata.
	req := documents.listEventsReq
	if req.GetEntityType() != "requisition" || req.GetEntityId() != "r-1" ||
		req.GetLimit() != 50 || req.GetNextCursor() != "abc" {
		t.Fatalf("documents request = %+v", req)
	}
	md, ok := metadata.FromOutgoingContext(documents.lastCtx)
	if !ok || len(md.Get(rbac.MDTenantID)) != 1 || md.Get(rbac.MDTenantID)[0] != "tenant-9" {
		t.Fatal("tenant metadata not propagated to documents")
	}
	if len(md.Get(rbac.MDRole)) != 1 || md.Get(rbac.MDRole)[0] != rbac.RoleBuyer {
		t.Fatalf("role metadata = %v", md.Get(rbac.MDRole))
	}
}

func TestListEventsDefaultsLimitAndTrimsParams(t *testing.T) {
	app, documents, header, _ := eventsTestApp(t)
	documents.listEventsResp = &documentsv1.ListEventsResponse{}

	resp, _ := do(t, app, http.MethodGet,
		"/api/v1/events?entityType=%20requisition%20&entityId=&limit=notanumber&cursor=%20%20",
		header.Get("Authorization"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	req := documents.listEventsReq
	if req.GetEntityType() != "requisition" || req.GetEntityId() != "" ||
		req.GetLimit() != 0 || req.GetNextCursor() != "" {
		t.Fatalf("documents request = %+v", req)
	}
}

func TestListEventsValidationErrors(t *testing.T) {
	app, documents, header, _ := eventsTestApp(t)
	for _, target := range []struct {
		code string
		want string
	}{
		{"events_filter_invalid", "entityType and entityId must both be set or both empty"},
		{"events_cursor_invalid", "cursor is not a valid continuation token"},
	} {
		documents.listEventsResp, documents.listEventsErr = nil, status.Error(codes.InvalidArgument, target.code)
		resp, body := do(t, app, http.MethodGet, "/api/v1/events?entityType=requisition", header.Get("Authorization"))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", target.code, resp.StatusCode)
		}
		if body["code"] != target.code || body["error"] != target.want {
			t.Fatalf("%s: body = %v", target.code, body)
		}
	}
}

func TestListEventsRequirePermission(t *testing.T) {
	app, documents, header, sign := eventsTestApp(t)

	// A role outside the rbac matrix holds nothing — every seeded role has
	// requisition:read, so an unknown role is the denial case (dev-note 042).
	noRole := header.Clone()
	noRole.Set("Authorization", "Bearer "+sign("auditor"))
	resp, body := do(t, app, http.MethodGet, "/api/v1/events", noRole.Get("Authorization"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown role: status = %d, want 403", resp.StatusCode)
	}
	if body["code"] != "permission_denied" {
		t.Fatalf("body = %v", body)
	}
	if documents.lastCtx != nil {
		t.Fatal("documents must not be called without requisition:read")
	}

	// Missing bearer token: 401 before any documents call.
	resp, _ = do(t, app, http.MethodGet, "/api/v1/events", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}
}

// TestEventsRegisteredThroughNew pins the wiring in server.New: the route is
// live when WithDocuments is set, with no procurement client at all.
func TestEventsRegisteredThroughNew(t *testing.T) {
	t.Helper()
	documents := &fakeDocumentsClient{
		listEventsResp: &documentsv1.ListEventsResponse{},
	}
	pub, priv, _ := jwt.GenerateKey()
	kid := jwt.Kid(pub)
	verifier := token.NewVerifier(&staticFetcher{keys: []token.Key{{Kid: kid, Ed25519: pub, Current: true}}}, time.Hour)
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)) // keep test output clean
	app := New(log, verifier, WithDocuments(documents))
	t.Cleanup(func() { _ = app.Shutdown() })

	now := time.Now()
	bearer, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "user-9", TenantID: "tenant-9", Roles: []string{rbac.RoleBuyer},
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	resp, body := do(t, app, http.MethodGet, "/api/v1/events", "Bearer "+bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %v", resp.StatusCode, body)
	}
	if _, ok := body["events"]; !ok {
		t.Fatalf("body = %v", body)
	}
}
