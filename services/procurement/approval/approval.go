// Package approval implements the approval engine on top of the requisition
// aggregate: tenant-configurable rules (role + threshold + currency +
// step_no), the matrix evaluation that materializes rules into approval
// steps at submit, and the sequential decide flow (approve/reject with
// reason, self-approval ban, role enforcement, reject short-circuits the
// remaining steps). Pure domain decisions live here; the store (store.go)
// adapts them to PostgreSQL inside the requisition's transactions.
package approval

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/luksluki/eprocurement/services/procurement/requisition"
)

// Approval step lifecycle statuses. Steps are created pending; a decision
// makes one approved or rejected; a reject marks every later pending step
// skipped so the flow visibly stops.
const (
	StepPending  = "pending"
	StepApproved = "approved"
	StepRejected = "rejected"
	StepSkipped  = "skipped"
)

// Decision values (wire input, normalized to lowercase at the edge).
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// Domain event types, one per step decision. The requisition-level
// requisition_approved / requisition_rejected events already exist in the
// requisition package; this package records only the step events. The event
// history (#43) and the outbox (#45) consume them later.
const (
	EventStepApproved = "approval_step_approved"
	EventStepRejected = "approval_step_rejected"
)

// The role catalog mirrors the fixed six roles seeded in identity.roles and
// enforced by internal/platform/rbac (admin, requester, approver, buyer,
// finance, viewer). The mirror keeps this domain package free of platform
// imports, same as the currency allowlist below.
const roleAdmin = "admin"

var allowedRoles = map[string]bool{
	"admin": true, "requester": true, "approver": true,
	"buyer": true, "finance": true, "viewer": true,
}

// The currency allowlist mirrors the requisition domain's {IDR, USD} (FR-03);
// a rule only ever matches a requisition in its own currency.
var allowedCurrencies = map[string]bool{
	requisition.CurrencyIDR: true,
	requisition.CurrencyUSD: true,
}

// CurrencyAllowed reports whether c is on the rule allowlist.
func CurrencyAllowed(c string) bool { return allowedCurrencies[c] }

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged (same contract as the vendor and requisition
// domains).
var (
	ErrRulesMissing         = errors.New("approval_rules_missing")
	ErrRuleCurrencyInvalid  = errors.New("approval_rule_currency_invalid")
	ErrRuleRoleInvalid      = errors.New("approval_rule_role_invalid")
	ErrRuleThresholdInvalid = errors.New("approval_rule_threshold_invalid")
	ErrRuleStepNoInvalid    = errors.New("approval_rule_step_no_invalid")
	ErrRuleStepNoDuplicate  = errors.New("approval_rule_step_no_duplicate")
	ErrDecisionInvalid      = errors.New("approval_decision_invalid")
	ErrReasonRequired       = errors.New("approval_reason_required")
	ErrReasonTooLong        = errors.New("approval_reason_invalid")
	ErrSelfForbidden        = errors.New("approval_self_forbidden")
	ErrRoleForbidden        = errors.New("approval_role_forbidden")
	ErrStepNotCurrent       = errors.New("approval_step_not_current")
	ErrStepNotFound         = errors.New("approval_step_not_found")
	ErrStepIDInvalid        = errors.New("approval_step_id_invalid")
)

// maxReasonLen bounds the decision reason. Checked at the edge and again in
// the service layer (defense in depth), not in this pure function set.
const MaxReasonLen = 1000

// Rule is one tenant approval rule: requisitions in Currency totalling at
// least ThresholdCents must collect a decision from a holder of RoleRequired
// at position StepNo. Rules are live configuration; Step is the snapshot a
// submit takes of the rules that matched.
type Rule struct {
	ID             string
	Currency       string
	StepNo         int32
	RoleRequired   string
	ThresholdCents int64
}

// Step is one materialized approval step of one requisition.
type Step struct {
	ID             string
	RequisitionID  string
	StepNo         int32
	RoleRequired   string
	ThresholdCents int64
	Status         string
	DecidedBy      string
	DecidedAt      *time.Time
	Reason         string

	events []Event
}

// Event is a domain event recorded on the step aggregate; the shape mirrors
// the requisition aggregate's events.
type Event struct {
	Type       string
	OccurredAt time.Time
	Actor      string
}

// Events returns a copy of the recorded step events, oldest first.
func (s *Step) Events() []Event { return append([]Event(nil), s.events...) }

// PullEvents returns the recorded events and clears the log (outbox #45).
func (s *Step) PullEvents() []Event {
	events := s.events
	s.events = nil
	return events
}

// ValidateRuleSet checks a candidate rule set for the configure API:
// currency on the allowlist, role in the fixed catalog, threshold >= 0,
// step_no > 0, and step_no unique per currency. The set may be empty —
// deleting every rule is a legal configuration (submits then fail with
// ErrRulesMissing).
func ValidateRuleSet(rules []Rule) error {
	type currencyStep struct {
		currency string
		stepNo   int32
	}
	seen := map[currencyStep]bool{}
	for _, r := range rules {
		if !allowedCurrencies[r.Currency] {
			return ErrRuleCurrencyInvalid
		}
		if !allowedRoles[r.RoleRequired] {
			return ErrRuleRoleInvalid
		}
		if r.ThresholdCents < 0 {
			return ErrRuleThresholdInvalid
		}
		if r.StepNo <= 0 {
			return ErrRuleStepNoInvalid
		}
		key := currencyStep{r.Currency, r.StepNo}
		if seen[key] {
			return ErrRuleStepNoDuplicate
		}
		seen[key] = true
	}
	return nil
}

// Materialize evaluates the tenant's rules against a submitted requisition
// (matrix model): EVERY rule whose currency matches and whose threshold is
// at or below the total (inclusive lower bound) becomes one approval step —
// not just the highest threshold. Steps are returned ordered by step_no; the
// caller (the requisition store's submit transaction) persists them as the
// requisition's snapshot. No matching rule is ErrRulesMissing — the caller
// must reject the submit.
func Materialize(rules []Rule, requisitionID, currency string, totalCents int64) ([]Step, error) {
	steps := []Step{}
	for _, r := range rules {
		if r.Currency != currency || r.ThresholdCents > totalCents {
			continue
		}
		steps = append(steps, Step{
			RequisitionID:  requisitionID,
			StepNo:         r.StepNo,
			RoleRequired:   r.RoleRequired,
			ThresholdCents: r.ThresholdCents,
			Status:         StepPending,
		})
	}
	if len(steps) == 0 {
		return nil, ErrRulesMissing
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].StepNo < steps[j].StepNo })
	return steps, nil
}

// DecisionInput is one decide call. Decision is already normalized
// (trim + lowercase) and Reason trimmed by the caller; the reason length is
// bounded at the edge and the service layer before reaching here.
type DecisionInput struct {
	StepID     string
	Decision   string
	Reason     string
	ActorID    string
	ActorRoles []string
	Now        time.Time
}

// Decide applies one decision to the current step of a submitted
// requisition. steps must be the requisition's steps; req is the locked
// parent aggregate (the store holds the requisition row lock while this
// runs). Checks, in order:
//
//  1. the step belongs to this requisition (ErrStepNotFound);
//  2. decision shape — approve|reject, non-blank reason on reject
//     (ErrDecisionInvalid, ErrReasonRequired);
//  3. self-approval ban — the decider must not be the requester, with no
//     admin exception (agent decision #2 on issue #30) (ErrSelfForbidden);
//  4. role enforcement — the decider holds step.RoleRequired, admins
//     excepted, mirroring rbac.Check and requireOwner (ErrRoleForbidden);
//  5. currency of the flow — the requisition is submitted and this step is
//     its smallest pending step_no (ErrStepNotCurrent).
//
// Applying: the step becomes approved/rejected with its decision fields and
// one step event; a reject also marks every later pending step skipped and
// rejects the requisition (terminal); approving the last pending step
// approves the requisition. The updated steps are returned (the input slice
// is left untouched).
func Decide(req *requisition.Requisition, steps []Step, in DecisionInput) ([]Step, error) {
	idx := -1
	for i := range steps {
		if steps[i].ID == in.StepID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrStepNotFound
	}

	switch in.Decision {
	case DecisionApprove, DecisionReject:
	default:
		return nil, ErrDecisionInvalid
	}
	if in.Decision == DecisionReject && strings.TrimSpace(in.Reason) == "" {
		return nil, ErrReasonRequired
	}

	// Self-approval is forbidden for everyone, admins included.
	if in.ActorID != "" && in.ActorID == req.RequesterID {
		return nil, ErrSelfForbidden
	}
	// Role enforcement with the admin exemption (rbac.Check / requireOwner
	// precedent): an admin may decide any step, anyone else needs the
	// step's role.
	if !hasRole(in.ActorRoles, roleAdmin) && !hasRole(in.ActorRoles, steps[idx].RoleRequired) {
		return nil, ErrRoleForbidden
	}

	// Sequential engine: only the smallest pending step_no of a submitted
	// requisition is decidable.
	if req.Status != requisition.StatusSubmitted || steps[idx].Status != StepPending {
		return nil, ErrStepNotCurrent
	}
	for i := range steps {
		if steps[i].Status == StepPending && steps[i].StepNo < steps[idx].StepNo {
			return nil, ErrStepNotCurrent
		}
	}

	out := append([]Step(nil), steps...)
	step := &out[idx]
	switch in.Decision {
	case DecisionApprove:
		step.Status = StepApproved
		step.events = append(step.events, Event{Type: EventStepApproved, OccurredAt: in.Now, Actor: in.ActorID})
	case DecisionReject:
		step.Status = StepRejected
		step.events = append(step.events, Event{Type: EventStepRejected, OccurredAt: in.Now, Actor: in.ActorID})
	}
	step.DecidedBy = in.ActorID
	at := in.Now
	step.DecidedAt = &at
	step.Reason = in.Reason

	// A reject ends the flow: later pending steps are skipped (visible
	// stop, no decision recorded on them) and the requisition is rejected.
	if in.Decision == DecisionReject {
		for i := range out {
			if out[i].StepNo > step.StepNo && out[i].Status == StepPending {
				out[i].Status = StepSkipped
			}
		}
		if err := req.Reject(in.Now, in.ActorID); err != nil {
			return nil, err
		}
		return out, nil
	}

	// Approving the last pending step completes the flow.
	remaining := false
	for i := range out {
		if out[i].Status == StepPending {
			remaining = true
			break
		}
	}
	if !remaining {
		if err := req.Approve(in.Now, in.ActorID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
