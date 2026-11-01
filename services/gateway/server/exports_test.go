package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// exportTestApp wires the export routes against fakes and returns a
// finance header plus a signer for tokens with other roles. procurement is
// nil here: the GET-only surface (the #49 shape) — the trigger tests wire
// their own app with exportTriggerTestApp.
func exportTestApp(t *testing.T) (*fiber.App, *fakeDocumentsClient, http.Header, func(roles ...string) string) {
	t.Helper()
	documents := &fakeDocumentsClient{}
	app, verifier, priv, kid := newTestApp(t)
	registerExportRoutes(app, verifier, documents, nil)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-49", TenantID: "tenant-49", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleFinance))
	return app, documents, header, sign
}

// TestExportRunsRoute lists the run history: newest records pass through
// with the missed flag and the failed run's error message surfaced, the
// limit forwards, and the tenant rides as gRPC metadata.
func TestExportRunsRoute(t *testing.T) {
	app, documents, header, _ := exportTestApp(t)
	documents.listRunsResp = &documentsv1.ListExportRunsResponse{Runs: []*documentsv1.ExportRun{
		{
			RunId: "run-1", TenantId: "tenant-49", ScheduledFor: "2026-10-28T01:00:00Z",
			Status: "success", RowCount: 7,
			StartedAt: "2026-10-28T01:00:00Z", FinishedAt: "2026-10-28T01:00:02Z",
		},
		{
			RunId: "run-2", TenantId: "tenant-49", ScheduledFor: "2026-10-27T01:00:00Z",
			Status: "failed", MissedPrevious: true, ErrorMessage: "object store unreachable",
			StartedAt: "2026-10-27T01:00:00Z", FinishedAt: "2026-10-27T01:00:01Z",
		},
	}}

	resp := doReq(t, app, header, http.MethodGet, "/api/v1/exports/runs?limit=50", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if len(out.Runs) != 2 || out.Runs[0]["run_id"] != "run-1" {
		t.Fatalf("runs = %s", body)
	}
	if out.Runs[1]["missed_previous"] != true || out.Runs[1]["error_message"] != "object store unreachable" {
		t.Fatalf("failed run fields = %s", body)
	}
	if documents.listRunsReq.GetLimit() != 50 {
		t.Fatalf("limit forwarded = %d, want 50", documents.listRunsReq.GetLimit())
	}
	if documents.lastCtx == nil {
		t.Fatal("documents not called")
	}
}

// TestExportRunsRouteRequiresPermission: export:run is finance/admin — a
// buyer is refused at the edge with the machine-readable reason, before
// any documents call.
func TestExportRunsRouteRequiresPermission(t *testing.T) {
	app, documents, header, sign := exportTestApp(t)
	buyerHeader := header.Clone()
	buyerHeader.Set("Authorization", "Bearer "+sign(rbac.RoleBuyer))

	for _, path := range []string{
		"/api/v1/exports/runs",
		"/api/v1/exports/runs/run-1/download-url",
	} {
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
		if out.Code != "permission_denied" || out.Required != "export:run" {
			t.Fatalf("body = %s", body)
		}
	}
	if documents.lastCtx != nil {
		t.Fatal("documents must not be called for a denied role")
	}
}

// TestExportDownloadURLRoute: the happy path wraps the presigned URL in
// the download envelope (the attachment shape); documents' machine codes
// ride through — cross-tenant 404, failed run 412, malformed id 400.
func TestExportDownloadURLRoute(t *testing.T) {
	app, documents, header, _ := exportTestApp(t)

	documents.exportDownloadResp = &documentsv1.GetExportDownloadUrlResponse{
		Run: &documentsv1.ExportRun{
			RunId: "run-1", TenantId: "tenant-49", Status: "success", RowCount: 7,
			ScheduledFor: "2026-10-28T01:00:00Z",
		},
		DownloadUrl: "http://minio:9000/exports/tenant-49/2026-10-28/purchase-orders.csv?X-Amz-Signature=sig",
		ExpiresAt:   "2026-10-28T10:15:00Z",
	}
	resp := doReq(t, app, header, http.MethodGet, "/api/v1/exports/runs/run-1/download-url", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download-url status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		RunID    string `json:"run_id"`
		Status   string `json:"status"`
		Download struct {
			URL       string `json:"url"`
			ExpiresAt string `json:"expires_at"`
		} `json:"download"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if !strings.Contains(out.Download.URL, "X-Amz-Signature") || out.Download.ExpiresAt == "" {
		t.Fatalf("download envelope = %s", body)
	}
	if out.RunID != "run-1" || out.Status != "success" {
		t.Fatalf("run envelope = %s (flat shape, the attachment convention)", body)
	}
	if documents.exportDownloadReq.GetRunId() != "run-1" {
		t.Fatalf("run id forwarded = %s", documents.exportDownloadReq.GetRunId())
	}

	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"cross-tenant run", status.Error(codes.NotFound, "export_run_not_found"), http.StatusNotFound, "export_run_not_found"},
		{"failed run", status.Error(codes.FailedPrecondition, "export_not_succeeded"), http.StatusPreconditionFailed, "export_not_succeeded"},
		{"malformed id", status.Error(codes.InvalidArgument, "export_run_id_invalid"), http.StatusBadRequest, "export_run_id_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			documents.exportDownloadResp = nil
			documents.exportDownloadErr = tc.err
			resp := doReq(t, app, header, http.MethodGet, "/api/v1/exports/runs/run-1/download-url", "")
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			body, _ := io.ReadAll(resp.Body)
			var out struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("unmarshal %s: %v", body, err)
			}
			if out.Code != tc.code {
				t.Fatalf("code = %s, want %s", out.Code, tc.code)
			}
		})
	}
}

// fakeExportTriggerClient satisfies ProcurementClient (the embedded
// requisition fake covers the rest) and records what the gateway forwarded
// to the trigger call.
type fakeExportTriggerClient struct {
	*fakeRequisitionClient

	triggerCtx  context.Context
	triggerResp *procurementv1.TriggerExportResponse
	triggerErr  error
	triggerHits int
}

func (f *fakeExportTriggerClient) TriggerExport(ctx context.Context, in *procurementv1.TriggerExportRequest, opts ...grpc.CallOption) (*procurementv1.TriggerExportResponse, error) {
	f.triggerHits++
	f.triggerCtx = ctx
	if f.triggerErr != nil {
		return nil, f.triggerErr
	}
	return f.triggerResp, nil
}

// exportTriggerTestApp wires the export surface with BOTH backends and
// returns an admin header plus a signer for other roles.
func exportTriggerTestApp(t *testing.T) (*fiber.App, *fakeDocumentsClient, *fakeExportTriggerClient, http.Header, func(roles ...string) string) {
	t.Helper()
	documents := &fakeDocumentsClient{}
	procurement := &fakeExportTriggerClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		triggerResp:           &procurementv1.TriggerExportResponse{EventId: "ev-53"},
	}
	app, verifier, priv, kid := newTestApp(t)
	registerExportRoutes(app, verifier, documents, procurement)

	sign := func(roles ...string) string {
		t.Helper()
		now := time.Now()
		token, err := jwt.Sign(priv, kid, auth.AccessClaims{
			Sub: "user-53", TenantID: "tenant-53", Roles: roles,
			IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
		}, now)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return token
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+sign(rbac.RoleAdmin))
	return app, documents, procurement, header, sign
}

// TestTriggerExportRoute: an admin POST /api/v1/exports answers 202 with
// the queued-event envelope, and the caller identity rides as gRPC
// metadata. Procurement failures map through the shared gRPC error shape.
func TestTriggerExportRoute(t *testing.T) {
	app, _, procurement, header, _ := exportTriggerTestApp(t)

	resp := doReq(t, app, header, http.MethodPost, "/api/v1/exports", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Status  string `json:"status"`
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if out.Status != "requested" || out.EventID != "ev-53" {
		t.Fatalf("body = %s, want {status: requested, event_id: ev-53}", body)
	}
	if procurement.triggerHits != 1 || procurement.triggerCtx == nil {
		t.Fatalf("procurement called %d times, want exactly 1 with context", procurement.triggerHits)
	}
	md, ok := metadata.FromOutgoingContext(procurement.triggerCtx)
	if !ok {
		t.Fatal("no outgoing metadata on the trigger call")
	}
	if got := md.Get("x-tenant-id"); len(got) != 1 || got[0] != "tenant-53" {
		t.Fatalf("x-tenant-id = %v, want the claims tenant", got)
	}

	// A backend failure surfaces through the shared mapping (502 for
	// Unavailable — the default arm, no export-specific machine codes).
	procurement.triggerErr = status.Error(codes.Unavailable, "procurement down")
	resp = doReq(t, app, header, http.MethodPost, "/api/v1/exports", "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unavailable status = %d, want 502", resp.StatusCode)
	}
}

// TestTriggerExportRouteRequiresPermission: export:trigger is admin-only —
// finance (which holds export:run, the read permission from #49) and buyer
// are refused at the edge with the machine-readable reason, before any
// procurement call; anonymous calls are 401 from the Bearer middleware.
func TestTriggerExportRouteRequiresPermission(t *testing.T) {
	app, _, procurement, header, sign := exportTriggerTestApp(t)

	for _, role := range []string{rbac.RoleFinance, rbac.RoleBuyer} {
		roleHeader := header.Clone()
		roleHeader.Set("Authorization", "Bearer "+sign(role))
		resp := doReq(t, app, roleHeader, http.MethodPost, "/api/v1/exports", "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("trigger as %s: status = %d, want 403", role, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		var out struct {
			Code     string `json:"code"`
			Required string `json:"required"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		if out.Code != "permission_denied" || out.Required != "export:trigger" {
			t.Fatalf("trigger as %s: body = %s, want export:trigger", role, body)
		}
	}
	if procurement.triggerHits != 0 {
		t.Fatalf("procurement must not be called for denied roles (hits = %d)", procurement.triggerHits)
	}

	resp := doReq(t, app, http.Header{}, http.MethodPost, "/api/v1/exports", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: status = %d, want 401", resp.StatusCode)
	}
	if procurement.triggerHits != 0 {
		t.Fatalf("procurement must not be called anonymously (hits = %d)", procurement.triggerHits)
	}
}

// TestExportRoutesWithoutProcurement: an app wired without procurement
// (the #49 shape) exposes the GET routes only — POST /api/v1/exports is
// 404, not a panic or a 500 on a nil interface.
func TestExportRoutesWithoutProcurement(t *testing.T) {
	app, documents, header, _ := exportTestApp(t)
	documents.listRunsResp = &documentsv1.ListExportRunsResponse{Runs: []*documentsv1.ExportRun{
		{RunId: "run-1", TenantId: "tenant-49", Status: "success", Trigger: "manual"},
	}}

	resp := doReq(t, app, header, http.MethodGet, "/api/v1/exports/runs", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (GET half unaffected)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"trigger":"manual"`) {
		t.Fatalf("body = %s, want the trigger field on the wire", body)
	}

	resp = doReq(t, app, header, http.MethodPost, "/api/v1/exports", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("post without procurement: status = %d, want 404", resp.StatusCode)
	}
}

// TestExportRoutesWithoutDocuments: an app wired without documents exposes
// the trigger only — the GET routes are 404. No route registration panics
// on the nil documents interface.
func TestExportRoutesWithoutDocuments(t *testing.T) {
	procurement := &fakeExportTriggerClient{
		fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}},
		triggerResp:           &procurementv1.TriggerExportResponse{EventId: "ev-53"},
	}
	app, verifier, priv, kid := newTestApp(t)
	registerExportRoutes(app, verifier, nil, procurement)

	now := time.Now()
	token, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "user-53", TenantID: "tenant-53", Roles: []string{rbac.RoleAdmin},
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	resp := doReq(t, app, header, http.MethodPost, "/api/v1/exports", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202", resp.StatusCode)
	}
	resp = doReq(t, app, header, http.MethodGet, "/api/v1/exports/runs", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list without documents: status = %d, want 404", resp.StatusCode)
	}
}
