package server

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerApprovalRoutes wires the approval surface: the decide endpoint
// (PRD §15, approver/admin at both RBAC layers) and the admin-only rule
// configuration endpoints.
func registerApprovalRoutes(app *fiber.App, verifier *token.Verifier, client ProcurementClient) {
	decisions := app.Group("/api/v1/approvals", bearerAuth(verifier))
	decisions.Post("/:stepId/decide", RequirePermission(rbac.ApprovalDecide), decideApproval(client))

	rules := app.Group("/api/v1/approval-rules", bearerAuth(verifier))
	rules.Get("/", RequirePermission(rbac.ApprovalConfigure), getApprovalRules(client))
	rules.Put("/", RequirePermission(rbac.ApprovalConfigure), replaceApprovalRules(client))
}

// decideBody is the decide payload; decision is normalized (trim +
// lowercase) and reason trimmed at the edge, then re-validated in the
// service layer (defense in depth).
type decideBody struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// decideApproval serves POST /api/v1/approvals/{stepId}/decide. The step id
// comes from the submit/decide responses' approval_steps (there is no read
// endpoint yet — #31/#32-34). Edge validation: decision ∈ {approve, reject},
// reject requires a non-blank reason, reason ≤ 1000 chars.
func decideApproval(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body decideBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		body.Decision = strings.ToLower(strings.TrimSpace(body.Decision))
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Decision != "approve" && body.Decision != "reject" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "decision must be approve or reject",
				"code":  "approval_decision_invalid",
			})
		}
		if body.Decision == "reject" && body.Reason == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "a reject requires a reason",
				"code":  "approval_reason_required",
			})
		}
		if len(body.Reason) > maxReasonLen {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "reason must be at most 1000 characters",
				"code":  "approval_reason_invalid",
			})
		}
		resp, err := client.DecideApproval(forwardContext(c, claims), &procurementv1.DecideApprovalRequest{
			StepId:   c.Params("stepId"),
			Decision: body.Decision,
			Reason:   body.Reason,
		})
		if err != nil {
			return approvalError(c, err)
		}
		return c.JSON(requisitionJSON(resp.GetRequisition()))
	}
}

// maxReasonLen mirrors the service-layer bound (approval.MaxReasonLen).
const maxReasonLen = 1000

// ruleBody is one rule of the replace payload.
type ruleBody struct {
	Currency       string `json:"currency"`
	StepNo         int32  `json:"step_no"`
	RoleRequired   string `json:"role_required"`
	ThresholdCents int64  `json:"threshold_cents"`
}

type rulesBody struct {
	Rules []ruleBody `json:"rules"`
}

// getApprovalRules serves GET /api/v1/approval-rules (admin-only).
func getApprovalRules(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		resp, err := client.GetApprovalRules(forwardContext(c, claims), &procurementv1.GetApprovalRulesRequest{})
		if err != nil {
			return approvalError(c, err)
		}
		return c.JSON(fiber.Map{"rules": rulesJSON(resp.GetRules())})
	}
}

// replaceApprovalRules serves PUT /api/v1/approval-rules (admin-only): the
// payload is the tenant's complete new rule set, normalized at the edge
// (currency upper, role lower).
func replaceApprovalRules(client ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body rulesBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		rules := make([]*procurementv1.ApprovalRule, 0, len(body.Rules))
		for _, r := range body.Rules {
			rules = append(rules, &procurementv1.ApprovalRule{
				Currency:       strings.ToUpper(strings.TrimSpace(r.Currency)),
				StepNo:         r.StepNo,
				RoleRequired:   strings.ToLower(strings.TrimSpace(r.RoleRequired)),
				ThresholdCents: r.ThresholdCents,
			})
		}
		resp, err := client.ReplaceApprovalRules(forwardContext(c, claims), &procurementv1.ReplaceApprovalRulesRequest{Rules: rules})
		if err != nil {
			return approvalError(c, err)
		}
		return c.JSON(fiber.Map{"rules": rulesJSON(resp.GetRules())})
	}
}

func rulesJSON(rules []*procurementv1.ApprovalRule) []fiber.Map {
	out := make([]fiber.Map, 0, len(rules))
	for _, r := range rules {
		out = append(out, fiber.Map{
			"id":              r.GetId(),
			"currency":        r.GetCurrency(),
			"step_no":         r.GetStepNo(),
			"role_required":   r.GetRoleRequired(),
			"threshold_cents": r.GetThresholdCents(),
		})
	}
	return out
}

// approvalMessages renders each approval machine code as a human sentence.
var approvalMessages = map[string]string{
	"approval_rules_missing":          "no approval rule applies to this requisition; configure rules first",
	"approval_rule_currency_invalid":  "rule currency must be IDR or USD",
	"approval_rule_role_invalid":      "rule role must be one of the six fixed roles",
	"approval_rule_threshold_invalid": "rule threshold must be zero or more",
	"approval_rule_step_no_invalid":   "rule step_no must be greater than zero",
	"approval_rule_step_no_duplicate": "rule step_no must be unique per currency",
	"approval_decision_invalid":       "decision must be approve or reject",
	"approval_reason_required":        "a reject requires a reason",
	"approval_reason_invalid":         "reason must be at most 1000 characters",
	"approval_self_forbidden":         "you cannot decide your own requisition",
	"approval_role_forbidden":         "this step requires a role you do not hold",
	"approval_step_not_current":       "this step is not the current pending step",
	"approval_step_not_found":         "approval step not found",
	"approval_step_id_invalid":        "approval step id must be a UUID",
}

// approvalError renders procurement's approval failures through the shared
// gRPC→HTTP mapping with the machine code preserved.
func approvalError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, approvalMessages)
}
