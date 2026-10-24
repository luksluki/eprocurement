package approval

import (
	"errors"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/services/procurement/requisition"
)

var (
	testNow     = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	testTenant  = "aaaaaaaa-0000-0000-0000-000000000001"
	testUser    = "aaaaaaaa-0000-0000-0000-0000000000aa" // requester
	testDecider = "aaaaaaaa-0000-0000-0000-0000000000bb"
)

// matrixRules is one tenant's IDR chain plus an unrelated USD rule.
var matrixRules = []Rule{
	{ID: "r1", Currency: "IDR", StepNo: 2, RoleRequired: "finance", ThresholdCents: 10_000_000},
	{ID: "r2", Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 5_000_000},
	{ID: "r3", Currency: "USD", StepNo: 1, RoleRequired: "approver", ThresholdCents: 1},
}

// AC (matrix model, inclusive lower bound): every matching-currency rule at
// or below the total is materialized — not just the highest threshold —
// ordered by step_no; other-currency rules never match; no match at all is
// ErrRulesMissing.
func TestMaterialize(t *testing.T) {
	for name, tc := range map[string]struct {
		rules      []Rule
		totalCents int64
		wantErr    error
		wantSteps  []int32 // expected step_no sequence
	}{
		"total equals threshold is included (inclusive lower bound)": {
			rules: []Rule{{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 5_000_000}},
			// one line 1 x 5,000,000
			totalCents: 5_000_000, wantSteps: []int32{1},
		},
		"total one cent below threshold is excluded": {
			rules: []Rule{{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 5_000_000}},
			// 2 x 2,499,999 + 1 x 2 = 4,999,999+2 — use exact 4,999,999
			totalCents: 5_000_000 - 1, wantErr: ErrRulesMissing,
		},
		"total above two thresholds materializes both steps ordered by step_no": {
			rules:      matrixRules,
			totalCents: 15_450_000, wantSteps: []int32{1, 2},
		},
		"only the lower threshold matches": {
			rules:      matrixRules,
			totalCents: 7_000_000, wantSteps: []int32{1},
		},
		"other-currency rules are ignored": {
			rules:      []Rule{{Currency: "USD", StepNo: 1, RoleRequired: "approver", ThresholdCents: 1}},
			totalCents: 100, wantErr: ErrRulesMissing,
		},
		"no rules at all": {
			rules: nil, totalCents: 100, wantErr: ErrRulesMissing,
		},
	} {
		steps, err := Materialize(tc.rules, "req-1", "IDR", tc.totalCents)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected err %v", name, err)
			continue
		}
		if len(steps) != len(tc.wantSteps) {
			t.Errorf("%s: %d steps (%+v), want %d", name, len(steps), steps, len(tc.wantSteps))
			continue
		}
		for i, s := range steps {
			if s.StepNo != tc.wantSteps[i] {
				t.Errorf("%s: step[%d].step_no = %d, want %d", name, i, s.StepNo, tc.wantSteps[i])
			}
			if s.Status != StepPending || s.RequisitionID != "req-1" {
				t.Errorf("%s: step[%d] = %+v, want pending step of req-1", name, i, s)
			}
		}
		// The USD rule never leaks in even when everything matches.
		for _, s := range steps {
			if s.RoleRequired == "finance" && tc.totalCents < 10_000_000 {
				t.Errorf("%s: finance step materialized below its threshold", name)
			}
		}
	}
}

// AC (rule validation): currency on the allowlist, role in the fixed
// catalog, threshold >= 0, step_no > 0 and unique per currency.
func TestValidateRuleSet(t *testing.T) {
	valid := []Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
		{Currency: "IDR", StepNo: 2, RoleRequired: "finance", ThresholdCents: 10_000_000},
		{Currency: "USD", StepNo: 1, RoleRequired: "admin", ThresholdCents: 100},
	}
	if err := ValidateRuleSet(valid); err != nil {
		t.Fatalf("valid set: %v", err)
	}
	// The empty set is legal (deleting every rule).
	if err := ValidateRuleSet(nil); err != nil {
		t.Fatalf("empty set: %v", err)
	}
	for name, tc := range map[string]struct {
		rule    Rule
		wantErr error
	}{
		"bad currency":       {Rule{Currency: "EUR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0}, ErrRuleCurrencyInvalid},
		"bad role":           {Rule{Currency: "IDR", StepNo: 1, RoleRequired: "cto", ThresholdCents: 0}, ErrRuleRoleInvalid},
		"negative threshold": {Rule{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: -1}, ErrRuleThresholdInvalid},
		"zero step_no":       {Rule{Currency: "IDR", StepNo: 0, RoleRequired: "approver", ThresholdCents: 0}, ErrRuleStepNoInvalid},
	} {
		if err := ValidateRuleSet([]Rule{tc.rule}); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
		}
	}
	// Duplicate step_no per currency; the same step_no in another currency
	// is fine.
	if err := ValidateRuleSet([]Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 0},
		{Currency: "IDR", StepNo: 1, RoleRequired: "finance", ThresholdCents: 1},
	}); !errors.Is(err, ErrRuleStepNoDuplicate) {
		t.Errorf("duplicate: err = %v, want ErrRuleStepNoDuplicate", err)
	}
}

// submittedReq builds a submitted requisition for the decide tests; the
// caller supplies the total through the items.
func submittedReq(t *testing.T, totalCents int64) *requisition.Requisition {
	t.Helper()
	r, err := requisition.New(requisition.NewInput{
		TenantID: testTenant, Number: "PR-000001", RequesterID: testUser,
		Currency: "IDR",
		Items:    []requisition.Item{{Description: "Laptop", Qty: 1, UnitPriceCents: totalCents}},
		Now:      testNow, CreatedBy: testUser,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := r.Submit(testNow, testUser); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return r
}

// materialized builds a two-step flow (approver then finance) for a total
// that matches both thresholds, with IDs assigned like the store would.
func materialized(t *testing.T, req *requisition.Requisition) []Step {
	t.Helper()
	steps, err := Materialize(matrixRules, req.ID, req.Currency, req.TotalCents)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for i := range steps {
		steps[i].ID = "step-" + string(rune('0'+steps[i].StepNo))
	}
	return steps
}

func approveInput(stepID, actor string, roles ...string) DecisionInput {
	return DecisionInput{StepID: stepID, Decision: DecisionApprove, ActorID: actor, ActorRoles: roles, Now: testNow}
}

// AC (sequential engine): only the smallest pending step of a submitted
// requisition is decidable; approving the last step approves the
// requisition; events are recorded per decision.
func TestDecideApproveFlow(t *testing.T) {
	req := submittedReq(t, 15_450_000)
	steps := materialized(t, req)

	// Step 2 (finance) is not current while step 1 is pending.
	if _, err := Decide(req, steps, approveInput(steps[1].ID, testDecider, "finance")); !errors.Is(err, ErrStepNotCurrent) {
		t.Fatalf("decide step 2 first: err = %v, want ErrStepNotCurrent", err)
	}

	updated, err := Decide(req, steps, approveInput(steps[0].ID, testDecider, "approver"))
	if err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	if updated[0].Status != StepApproved || updated[1].Status != StepPending {
		t.Fatalf("after step 1: %+v", updated)
	}
	if req.Status != requisition.StatusSubmitted {
		t.Fatalf("mid-flow status = %s, want submitted", req.Status)
	}
	// Step event recorded on the decided aggregate only; fields set.
	evs := updated[0].Events()
	if len(evs) != 1 || evs[0].Type != EventStepApproved || evs[0].Actor != testDecider {
		t.Fatalf("step events = %+v", evs)
	}
	if updated[0].DecidedBy != testDecider || updated[0].DecidedAt == nil {
		t.Fatalf("decision fields = %+v", updated[0])
	}
	if len(updated[1].Events()) != 0 {
		t.Fatalf("undecided step recorded events: %+v", updated[1].Events())
	}
	// The input slice is untouched (decide returns a copy).
	if steps[0].Status != StepPending {
		t.Fatal("input slice mutated")
	}

	// Approving the last step completes the flow: requisition_approved on
	// the requisition aggregate, step event on the step.
	updated, err = Decide(req, updated, approveInput(updated[1].ID, testDecider, "finance"))
	if err != nil {
		t.Fatalf("approve step 2: %v", err)
	}
	if req.Status != requisition.StatusApproved {
		t.Fatalf("final status = %s, want approved", req.Status)
	}
	reqEvents := req.Events()
	if reqEvents[len(reqEvents)-1].Type != requisition.EventApproved {
		t.Fatalf("last requisition event = %+v, want requisition_approved", reqEvents[len(reqEvents)-1])
	}
	if updated[1].Status != StepApproved {
		t.Fatalf("step 2 = %+v", updated[1])
	}

	// Any further decision on the terminal flow is not current.
	if _, err := Decide(req, updated, approveInput(updated[1].ID, testDecider, "finance")); !errors.Is(err, ErrStepNotCurrent) {
		t.Fatalf("decide after terminal: err = %v, want ErrStepNotCurrent", err)
	}
}

// AC (reject stops the flow): the step is rejected, later pending steps are
// skipped, the requisition is rejected (terminal), and every later decision
// is ErrStepNotCurrent.
func TestDecideRejectFlow(t *testing.T) {
	req := submittedReq(t, 15_450_000)
	steps := materialized(t, req)

	in := DecisionInput{StepID: steps[0].ID, Decision: DecisionReject, Reason: "over budget", ActorID: testDecider, ActorRoles: []string{"approver"}, Now: testNow}
	updated, err := Decide(req, steps, in)
	if err != nil {
		t.Fatalf("reject step 1: %v", err)
	}
	if updated[0].Status != StepRejected || updated[0].Reason != "over budget" || updated[0].DecidedBy != testDecider {
		t.Fatalf("rejected step = %+v", updated[0])
	}
	if updated[1].Status != StepSkipped {
		t.Fatalf("downstream step = %s, want skipped", updated[1].Status)
	}
	if updated[1].DecidedBy != "" || updated[1].DecidedAt != nil {
		t.Fatalf("skipped step records a decision: %+v", updated[1])
	}
	if req.Status != requisition.StatusRejected {
		t.Fatalf("status = %s, want rejected", req.Status)
	}
	reqEvents := req.Events()
	if reqEvents[len(reqEvents)-1].Type != requisition.EventRejected {
		t.Fatalf("last requisition event = %+v, want requisition_rejected", reqEvents[len(reqEvents)-1])
	}
	evs := updated[0].Events()
	if len(evs) != 1 || evs[0].Type != EventStepRejected {
		t.Fatalf("step events = %+v", evs)
	}

	// Further decisions on any step of the dead flow are 409s (the actor
	// holds every role, so the role check passes and the state check fires).
	for _, s := range updated {
		in := DecisionInput{StepID: s.ID, Decision: DecisionApprove, ActorID: testDecider, ActorRoles: []string{"approver", "finance"}, Now: testNow}
		if _, err := Decide(req, updated, in); !errors.Is(err, ErrStepNotCurrent) {
			t.Fatalf("decide %s after reject: err = %v, want ErrStepNotCurrent", s.ID, err)
		}
	}
}

// AC (role enforcement, agent decision #3): the decider holds the step's
// role_required (admins excepted, rbac.Check / requireOwner precedent).
func TestDecideRoleEnforcement(t *testing.T) {
	req := submittedReq(t, 15_450_000)
	steps := materialized(t, req)

	// A pure approver cannot decide the finance step (role fires before the
	// currency check; this step is deliberately non-current so only the
	// role denial is asserted).
	if _, err := Decide(req, steps, approveInput(steps[1].ID, testDecider, "approver")); !errors.Is(err, ErrRoleForbidden) {
		t.Fatalf("approver on finance step: err = %v, want ErrRoleForbidden", err)
	}

	// Advance to step 2 (now current), then re-run the role matrix on it.
	after1, err := Decide(req, steps, approveInput(steps[0].ID, testDecider, "approver"))
	if err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	if _, err := Decide(req, after1, approveInput(after1[1].ID, testDecider, "approver")); !errors.Is(err, ErrRoleForbidden) {
		t.Fatalf("approver on current finance step: err = %v, want ErrRoleForbidden", err)
	}
	// A dual-role finance+approver user may.
	if _, err := Decide(req, after1, approveInput(after1[1].ID, testDecider, "finance", "approver")); err != nil {
		t.Fatalf("finance+approver on finance step: %v", err)
	}

	// An admin may decide any step (exemption mirrors rbac.Check).
	req2 := submittedReq(t, 15_450_000)
	steps2 := materialized(t, req2)
	after1b, err := Decide(req2, steps2, approveInput(steps2[0].ID, testDecider, "approver"))
	if err != nil {
		t.Fatalf("approve step 1 (admin case): %v", err)
	}
	if _, err := Decide(req2, after1b, approveInput(after1b[1].ID, testDecider, "admin")); err != nil {
		t.Fatalf("admin on finance step: %v", err)
	}
}

// AC (self-approval ban, agent decision #2): caller == requester_id is
// forbidden with NO admin exception.
func TestDecideSelfApprovalForbidden(t *testing.T) {
	req := submittedReq(t, 15_450_000) // requester: testUser
	steps := materialized(t, req)

	// The requester holds approver — still forbidden.
	if _, err := Decide(req, steps, approveInput(steps[0].ID, testUser, "approver")); !errors.Is(err, ErrSelfForbidden) {
		t.Fatalf("requester decide: err = %v, want ErrSelfForbidden", err)
	}
	// Even as admin — the ban is universal.
	if _, err := Decide(req, steps, approveInput(steps[0].ID, testUser, "admin")); !errors.Is(err, ErrSelfForbidden) {
		t.Fatalf("requester-admin decide: err = %v, want ErrSelfForbidden", err)
	}
	// A reject by the requester is equally forbidden.
	in := DecisionInput{StepID: steps[0].ID, Decision: DecisionReject, Reason: "x", ActorID: testUser, ActorRoles: []string{"admin"}, Now: testNow}
	if _, err := Decide(req, steps, in); !errors.Is(err, ErrSelfForbidden) {
		t.Fatalf("requester-admin reject: err = %v, want ErrSelfForbidden", err)
	}
	if req.Status != requisition.StatusSubmitted {
		t.Fatalf("failed self-approval mutated the flow: %s", req.Status)
	}
}

// AC (decision reasons): decision must be approve|reject; reject requires a
// non-blank reason; approve does not.
func TestDecideDecisionValidation(t *testing.T) {
	req := submittedReq(t, 15_450_000)
	steps := materialized(t, req)

	bad := approveInput(steps[0].ID, testDecider, "approver")
	bad.Decision = "maybe"
	if _, err := Decide(req, steps, bad); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("bad decision: err = %v, want ErrDecisionInvalid", err)
	}
	rejectNoReason := DecisionInput{StepID: steps[0].ID, Decision: DecisionReject, Reason: "   ", ActorID: testDecider, ActorRoles: []string{"approver"}, Now: testNow}
	if _, err := Decide(req, steps, rejectNoReason); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("reject without reason: err = %v, want ErrReasonRequired", err)
	}
	rejectBlank := rejectNoReason
	rejectBlank.Reason = ""
	if _, err := Decide(req, steps, rejectBlank); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("reject empty reason: err = %v, want ErrReasonRequired", err)
	}
	// Approve carries no reason requirement (an optional reason is kept).
	withReason := approveInput(steps[0].ID, testDecider, "approver")
	withReason.Reason = "looks fine"
	updated, err := Decide(req, steps, withReason)
	if err != nil || updated[0].Reason != "looks fine" {
		t.Fatalf("approve with reason = (%+v, %v)", updated[0], err)
	}
}

// Unknown step ids and non-submitted requisitions are distinguished errors.
func TestDecideLookupAndState(t *testing.T) {
	req := submittedReq(t, 15_450_000)
	steps := materialized(t, req)

	if _, err := Decide(req, steps, approveInput("no-such-step", testDecider, "approver")); !errors.Is(err, ErrStepNotFound) {
		t.Fatalf("unknown step: err = %v, want ErrStepNotFound", err)
	}

	// A draft requisition (steps not yet materializable in practice, but
	// the guard is domain-owned) cannot be decided.
	draft, err := requisition.New(requisition.NewInput{
		TenantID: testTenant, Number: "PR-000002", RequesterID: testUser, Currency: "IDR",
		Items: []requisition.Item{{Description: "x", Qty: 1, UnitPriceCents: 15_450_000}},
		Now:   testNow, CreatedBy: testUser,
	})
	if err != nil {
		t.Fatalf("new draft: %v", err)
	}
	draftSteps := append([]Step(nil), steps...)
	draftSteps[0].RequisitionID = draft.ID
	if _, err := Decide(draft, draftSteps, approveInput(draftSteps[0].ID, testDecider, "approver")); !errors.Is(err, ErrStepNotCurrent) {
		t.Fatalf("draft decide: err = %v, want ErrStepNotCurrent", err)
	}
}

// Events/PullEvents copy-and-clear semantics mirror the requisition
// aggregate.
func TestStepEventPull(t *testing.T) {
	req := submittedReq(t, 15_450_000)
	steps := materialized(t, req)
	updated, err := Decide(req, steps, approveInput(steps[0].ID, testDecider, "approver"))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got := updated[0].PullEvents(); len(got) != 1 {
		t.Fatalf("pull = %+v", got)
	}
	if got := updated[0].PullEvents(); len(got) != 0 {
		t.Fatalf("second pull = %+v", got)
	}
	if got := updated[0].Events(); len(got) != 0 {
		t.Fatalf("events after pull = %+v", got)
	}
}
