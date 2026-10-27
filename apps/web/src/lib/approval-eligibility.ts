// Pure UI-side gating for the requisition approve/reject actions (issue #34,
// AC "actions hidden when not permitted"). This mirrors — never replaces —
// the server's rules; procurement re-checks every gate inside the decide
// transaction (services/procurement/approval/approval.go) and the gateway
// enforces RequirePermission(approval:decide) first. The helper only decides
// whether the buttons RENDER; a lost race surfaces as a mapped 4xx
// (approval_step_not_current, …) from the server.

// approval:decide holders per internal/platform/rbac: approver explicitly,
// admin implicitly (rbac.Check grants admin every permission).
const ROLE_ADMIN = "admin";
const ROLE_APPROVER = "approver";

// Requisition status values (requisition.StatusSubmitted et al.).
const STATUS_SUBMITTED = "submitted";
// Approval step status values (approval.StepPending et al.).
const STEP_PENDING = "pending";

// The step fields the eligibility rules look at — a structural subset of the
// gateway's approval_steps rows (lib/gateway.ts ApprovalStep), kept minimal
// so the helper stays trivially testable.
export type EligibilityStep = {
  id: string;
  step_no: number;
  role_required: string;
  status: string;
};

export type EligibilityRequisition = {
  status: string;
  requester_id: string;
};

// holdsDecidePermission answers whether the role list grants approval:decide
// (approver, or admin holding everything implicitly).
export function holdsDecidePermission(roles: string[]): boolean {
  return roles.includes(ROLE_APPROVER) || roles.includes(ROLE_ADMIN);
}

// currentPendingStep mirrors the sequential engine: only the pending step
// with the smallest step_no of a submitted requisition is decidable
// (services/procurement/approval/approval.go Decide — the ErrStepNotCurrent
// loop). Returns null when nothing is pending.
export function currentPendingStep(steps: EligibilityStep[]): EligibilityStep | null {
  let current: EligibilityStep | null = null;
  for (const step of steps) {
    if (step.status !== STEP_PENDING) {
      continue;
    }
    if (!current || step.step_no < current.step_no) {
      current = step;
    }
  }
  return current;
}

// decidableStep returns the approval step the viewer may decide right now,
// or null when the actions must stay hidden. Gates, in the server's order:
// approval:decide permission → requisition submitted → self-approval
// forbidden (admins included) → the smallest pending step exists and its
// step_no is current → the viewer holds the step's role_required (admins
// exempt, mirroring rbac.Check / docs/rbac.md "R + approver, or an admin").
export function decidableStep(input: {
  roles: string[];
  userId: string;
  requisition: EligibilityRequisition;
  steps: EligibilityStep[];
}): EligibilityStep | null {
  const { roles, userId, requisition, steps } = input;
  if (!holdsDecidePermission(roles)) {
    return null;
  }
  if (requisition.status !== STATUS_SUBMITTED) {
    return null;
  }
  // Self-approval is forbidden for everyone, admins included.
  if (userId === requisition.requester_id) {
    return null;
  }
  const current = currentPendingStep(steps);
  if (!current) {
    return null;
  }
  if (!roles.includes(ROLE_ADMIN) && !roles.includes(current.role_required)) {
    return null;
  }
  return current;
}
