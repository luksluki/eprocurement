package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
)

// fakeApprovalClient satisfies the widened ProcurementClient (vendor +
// requisition via the embedded fakes) and records what the gateway
// forwarded.
type fakeApprovalClient struct {
	*fakeRequisitionClient

	decideReq  *procurementv1.DecideApprovalRequest
	decideResp *procurementv1.DecideApprovalResponse
	decideErr  error
	decideCtx  context.Context

	getRulesCalled bool
	rulesResp      *procurementv1.ApprovalRulesResponse

	replaceReq  *procurementv1.ReplaceApprovalRulesRequest
	replaceResp *procurementv1.ApprovalRulesResponse
	replaceErr  error
}

func (f *fakeApprovalClient) DecideApproval(ctx context.Context, in *procurementv1.DecideApprovalRequest, opts ...grpc.CallOption) (*procurementv1.DecideApprovalResponse, error) {
	f.decideCtx, f.decideReq = ctx, in
	return f.decideResp, f.decideErr
}

func (f *fakeApprovalClient) GetApprovalRules(ctx context.Context, in *procurementv1.GetApprovalRulesRequest, opts ...grpc.CallOption) (*procurementv1.ApprovalRulesResponse, error) {
	f.getRulesCalled = true
	if f.rulesResp == nil {
		return &procurementv1.ApprovalRulesResponse{}, nil
	}
	return f.rulesResp, nil
}

func (f *fakeApprovalClient) ReplaceApprovalRules(ctx context.Context, in *procurementv1.ReplaceApprovalRulesRequest, opts ...grpc.CallOption) (*procurementv1.ApprovalRulesResponse, error) {
	f.replaceReq = in
	return f.replaceResp, f.replaceErr
}

func approvalFake(resp *procurementv1.DecideApprovalResponse) *fakeApprovalClient {
	return &fakeApprovalClient{fakeRequisitionClient: &fakeRequisitionClient{fakeVendorClient: &fakeVendorClient{}}, decideResp: resp}
}

func sampleDecideResponse() *procurementv1.DecideApprovalResponse {
	return &procurementv1.DecideApprovalResponse{Requisition: &procurementv1.Requisition{
		Id: "pr-1", TenantId: "tenant-29", Number: "PR-000001", RequesterId: "user-29",
		Status: "approved", Currency: "IDR", TotalCents: 15_450_000,
		ApprovalSteps: []*procurementv1.ApprovalStep{
			{Id: "step-1", RequisitionId: "pr-1", StepNo: 1, RoleRequired: "approver", Status: "approved", DecidedBy: "user-30"},
			{Id: "step-2", RequisitionId: "pr-1", StepNo: 2, RoleRequired: "finance", Status: "approved", DecidedBy: "user-30"},
		},
	}}
}

// AC: the routes are wired through server.New with WithProcurement, the
// decide response embeds the requisition with its latest approval_steps
// (step ids in-band), and the verified claims ride along as gRPC metadata.
func TestApprovalRoutesThroughServerNew(t *testing.T) {
	fake := approvalFake(sampleDecideResponse())
	_, verifier, priv, kid := newTestApp(t)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithProcurement(fake))
	t.Cleanup(func() { _ = app.Shutdown() })

	resp, body := reqDo(t, app, "POST", "/api/v1/approvals/step-1/decide",
		reqTokenFor(t, priv, kid, rbac.RoleApprover), `{"decision":" APPROVE ","reason":"  ok  "}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decide status = %d, body = %v", resp.StatusCode, body)
	}
	if body["status"] != "approved" {
		t.Fatalf("decide body = %v", body)
	}
	steps, _ := body["approval_steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("approval_steps = %v", body["approval_steps"])
	}
	first, _ := steps[0].(map[string]any)
	if first["id"] != "step-1" || first["status"] != "approved" || first["decided_by"] != "user-30" {
		t.Fatalf("step = %v", first)
	}

	// The gateway normalized the decision and trimmed the reason before
	// forwarding; the claims traveled as metadata.
	if fake.decideReq.GetStepId() != "step-1" ||
		fake.decideReq.GetDecision() != "approve" || fake.decideReq.GetReason() != "ok" {
		t.Fatalf("forwarded decide = %+v", fake.decideReq)
	}
	md := metadataOf(t, fake.decideCtx)
	if md["x-tenant-id"] != "tenant-29" || md["x-user-id"] != "user-29" || md["x-role"] != rbac.RoleApprover {
		t.Fatalf("propagated metadata = %v", md)
	}
}

// AC (edge validation): a decision outside {approve, reject}, a reject
// without a reason, and an over-long reason are 400s with machine codes —
// without a procurement call.
func TestApprovalDecideEdgeValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantCode string
	}{
		"bad decision":     {`{"decision":"maybe"}`, "approval_decision_invalid"},
		"blank decision":   {`{"decision":"  "}`, "approval_decision_invalid"},
		"reject no reason": {`{"decision":"reject","reason":"   "}`, "approval_reason_required"},
		"reason too long":  {`{"decision":"approve","reason":"` + strings.Repeat("x", 1001) + `"}`, "approval_reason_invalid"},
	} {
		fake := approvalFake(sampleDecideResponse())
		app, verifier, priv, kid := newTestApp(t)
		registerApprovalRoutes(app, verifier, fake)
		token := reqTokenFor(t, priv, kid, rbac.RoleApprover)

		resp, body := reqDo(t, app, "POST", "/api/v1/approvals/step-1/decide", token, tc.body, nil)
		if resp.StatusCode != http.StatusBadRequest || body["code"] != tc.wantCode {
			t.Fatalf("%s: got (%d, %v), want (400, %s)", name, resp.StatusCode, body, tc.wantCode)
		}
		if fake.decideReq != nil {
			t.Fatalf("%s: procurement was called despite invalid input", name)
		}
	}
}

// AC (RBAC at the edge): a viewer cannot decide (approval:decide), and only
// admins configure rules (approval:configure) — 403 before any procurement
// call. An admin's PUT is forwarded with edge normalization.
func TestApprovalRoutesPermissionsAndRules(t *testing.T) {
	fake := approvalFake(nil)
	fake.replaceResp = &procurementv1.ApprovalRulesResponse{Rules: []*procurementv1.ApprovalRule{
		{Id: "rule-1", Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
	}}
	app, verifier, priv, kid := newTestApp(t)
	registerApprovalRoutes(app, verifier, fake)

	// Viewer: no approval:decide.
	resp, _ := reqDo(t, app, "POST", "/api/v1/approvals/step-1/decide",
		reqTokenFor(t, priv, kid, rbac.RoleViewer), `{"decision":"approve"}`, nil)
	if resp.StatusCode != http.StatusForbidden || fake.decideReq != nil {
		t.Fatalf("viewer decide = %d (called=%v), want 403 without a procurement call", resp.StatusCode, fake.decideReq != nil)
	}

	// Approver holds approval:decide but not approval:configure; requester
	// and finance hold neither. All are 403 before any procurement call.
	admin := reqTokenFor(t, priv, kid, rbac.RoleAdmin)
	for name, call := range map[string]func() (*http.Response, map[string]any){
		"get rules as approver": func() (*http.Response, map[string]any) {
			return reqDo(t, app, "GET", "/api/v1/approval-rules", reqTokenFor(t, priv, kid, rbac.RoleApprover), "", nil)
		},
		"put rules as approver": func() (*http.Response, map[string]any) {
			return reqDo(t, app, "PUT", "/api/v1/approval-rules", reqTokenFor(t, priv, kid, rbac.RoleApprover), `{"rules":[]}`, nil)
		},
		"get rules as requester": func() (*http.Response, map[string]any) {
			return reqDo(t, app, "GET", "/api/v1/approval-rules", reqTokenFor(t, priv, kid, rbac.RoleRequester), "", nil)
		},
		"put rules as finance": func() (*http.Response, map[string]any) {
			return reqDo(t, app, "PUT", "/api/v1/approval-rules", reqTokenFor(t, priv, kid, rbac.RoleFinance), `{"rules":[]}`, nil)
		},
	} {
		fake.getRulesCalled = false
		fake.replaceReq = nil
		resp, _ := call()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", name, resp.StatusCode)
		}
		if fake.getRulesCalled || fake.replaceReq != nil {
			t.Fatalf("%s: procurement was called despite missing approval:configure", name)
		}
	}

	// Admin may read and replace; the PUT payload is normalized at the edge
	// (currency upper, role lower) and echoed back with ids.
	resp, body := reqDo(t, app, "GET", "/api/v1/approval-rules", admin, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get rules = %d, body = %v", resp.StatusCode, body)
	}
	resp, body = reqDo(t, app, "PUT", "/api/v1/approval-rules", admin,
		`{"rules":[{"currency":" idr ","step_no":1,"role_required":" Approver ","threshold_cents":5000000}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin put rules = %d, body = %v", resp.StatusCode, body)
	}
	if len(fake.replaceReq.GetRules()) != 1 {
		t.Fatalf("forwarded rules = %+v", fake.replaceReq.GetRules())
	}
	fr := fake.replaceReq.GetRules()[0]
	if fr.GetCurrency() != "IDR" || fr.GetRoleRequired() != "approver" || fr.GetThresholdCents() != 5_000_000 {
		t.Fatalf("forwarded rule = %+v, want normalized IDR/approver", fr)
	}
	rules, _ := body["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("put body = %v", body)
	}
	rule, _ := rules[0].(map[string]any)
	if rule["id"] != "rule-1" || rule["currency"] != "IDR" {
		t.Fatalf("echoed rule = %v", rule)
	}
}

// The approval slice of the shared gRPC→HTTP mapping: 412 rules missing,
// 403 self/role, 409 not current, 404 step not found — machine codes always
// preserved in the body.
func TestApprovalRoutesErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"rules missing":    {status.Error(codes.FailedPrecondition, "approval_rules_missing"), http.StatusPreconditionFailed},
		"self forbidden":   {status.Error(codes.PermissionDenied, "approval_self_forbidden"), http.StatusForbidden},
		"role forbidden":   {status.Error(codes.PermissionDenied, "approval_role_forbidden"), http.StatusForbidden},
		"step not current": {status.Error(codes.Aborted, "approval_step_not_current"), http.StatusConflict},
		"step not found":   {status.Error(codes.NotFound, "approval_step_not_found"), http.StatusNotFound},
		"reason required":  {status.Error(codes.InvalidArgument, "approval_reason_required"), http.StatusBadRequest},
	} {
		fake := approvalFake(nil)
		fake.decideErr = tc.err
		app, verifier, priv, kid := newTestApp(t)
		registerApprovalRoutes(app, verifier, fake)
		token := reqTokenFor(t, priv, kid, rbac.RoleApprover)

		resp, body := reqDo(t, app, "POST", "/api/v1/approvals/step-1/decide", token, `{"decision":"approve"}`, nil)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status = %d, want %d", name, resp.StatusCode, tc.want)
		}
		if body["code"] != status.Convert(tc.err).Message() {
			t.Fatalf("%s: body %v lost the machine code", name, body)
		}
	}
}
