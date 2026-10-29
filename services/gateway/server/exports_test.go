package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// exportTestApp wires the export routes against fakes and returns a
// finance header plus a signer for tokens with other roles.
func exportTestApp(t *testing.T) (*fiber.App, *fakeDocumentsClient, http.Header, func(roles ...string) string) {
	t.Helper()
	documents := &fakeDocumentsClient{}
	app, verifier, priv, kid := newTestApp(t)
	registerExportRoutes(app, verifier, documents)

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
