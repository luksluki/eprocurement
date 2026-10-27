// Table-driven coverage for the UI gating helper (issue #34, AC "actions
// hidden when not permitted"). Each case mirrors a gate the server enforces
// (services/procurement/approval/approval.go Decide, docs/rbac.md line 33):
// the assertions here are about RENDERING, never authorization.
import { describe, expect, it } from "vitest";
import { currentPendingStep, decidableStep, holdsDecidePermission } from "./approval-eligibility";
import type { EligibilityStep } from "./approval-eligibility";

function step(overrides: Partial<EligibilityStep>): EligibilityStep {
  return {
    id: "step-1",
    step_no: 1,
    role_required: "approver",
    status: "pending",
    ...overrides,
  };
}

// The baseline viewer: a two-step submitted requisition, first step pending.
const baseSteps = (): EligibilityStep[] => [
  step({ id: "step-1", step_no: 1, role_required: "approver" }),
  step({ id: "step-2", step_no: 2, role_required: "admin", status: "approved", decided_by: "u-admin" }),
];

const base = {
  roles: ["approver"],
  userId: "u-approver",
  requisition: { status: "submitted", requester_id: "u-requester" },
  steps: baseSteps(),
};

describe("holdsDecidePermission", () => {
  it.each([
    { roles: ["approver"], expected: true },
    { roles: ["admin"], expected: true },
    { roles: ["requester", "approver"], expected: true },
    { roles: ["requester"], expected: false },
    { roles: ["viewer"], expected: false },
    { roles: ["buyer", "finance"], expected: false },
    { roles: [], expected: false },
  ])("roles $roles → $expected", ({ roles, expected }) => {
    expect(holdsDecidePermission(roles)).toBe(expected);
  });
});

describe("currentPendingStep", () => {
  it("returns the pending step with the smallest step_no", () => {
    const steps = [
      step({ id: "step-3", step_no: 3 }),
      step({ id: "step-1", step_no: 1 }),
      step({ id: "step-2", step_no: 2 }),
    ];
    expect(currentPendingStep(steps)?.id).toBe("step-1");
  });

  it("skips decided steps and returns null when nothing is pending", () => {
    const decided = [
      step({ id: "step-1", step_no: 1, status: "approved" }),
      step({ id: "step-2", step_no: 2, status: "skipped" }),
    ];
    expect(currentPendingStep(decided)).toBeNull();
    expect(currentPendingStep([])).toBeNull();
  });
});

describe("decidableStep", () => {
  it.each([
    {
      name: "approver holding the current step's role → shown",
      input: base,
      expected: "step-1",
    },
    {
      name: "viewer without approval:decide → hidden",
      input: { ...base, roles: ["viewer"] },
      expected: null,
    },
    {
      name: "requester without approval:decide → hidden",
      input: { ...base, roles: ["requester"] },
      expected: null,
    },
    {
      name: "self-approval forbidden, admins included → hidden",
      input: { ...base, roles: ["admin"], userId: "u-requester" },
      expected: null,
    },
    {
      name: "non-submitted requisition (approved) → hidden",
      input: { ...base, requisition: { status: "approved", requester_id: "u-requester" } },
      expected: null,
    },
    {
      name: "draft requisition → hidden",
      input: { ...base, requisition: { status: "draft", requester_id: "u-requester" } },
      expected: null,
    },
    {
      name: "all steps decided → hidden",
      input: { ...base, steps: [step({ status: "approved" })] },
      expected: null,
    },
    {
      name: "approver lacking the current step's role_required (buyer step) → hidden",
      input: { ...base, steps: [step({ role_required: "buyer" }), ...baseSteps().slice(1)] },
      expected: null,
    },
    {
      name: "admin exempt from role_required → shown",
      input: { ...base, roles: ["admin"], steps: [step({ role_required: "finance" }), ...baseSteps().slice(1)] },
      expected: "step-1",
    },
    {
      name: "multi-role holder (approver + step role) → shown",
      input: { ...base, roles: ["approver", "finance"], steps: [step({ role_required: "finance" }), ...baseSteps().slice(1)] },
      expected: "step-1",
    },
    {
      name: "only the smallest pending step is decidable — step 2's role holder still gets step 1 checked",
      input: {
        ...base,
        roles: ["approver"],
        steps: [step({ id: "step-1", step_no: 1, role_required: "finance" }), step({ id: "step-2", step_no: 2, role_required: "approver" })],
      },
      expected: null,
    },
    {
      name: "next step becomes decidable once earlier steps are approved",
      input: {
        ...base,
        steps: [step({ status: "approved", decided_by: "u-x" }), step({ id: "step-2", step_no: 2, role_required: "approver" })],
      },
      expected: "step-2",
    },
  ])("$name", ({ input, expected }) => {
    const result = decidableStep(input);
    expect(result ? result.id : null).toBe(expected);
  });
});
