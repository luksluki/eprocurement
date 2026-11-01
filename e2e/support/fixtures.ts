// Deterministic fixture constants for the #59 happy-path e2e — the
// maintainer's decision on issue #59: the spec owns its fixtures instead of
// `make seed` (the seed writes only the isolated demo.* schema that no
// service reads; onboarding §5.1). Everything here targets throwaway local
// stacks (compose CI runners, a dev laptop), so the values are fixtures,
// not secrets — the same shape as onboarding's documented `demo-password`.
//
// The user/tenant UUIDs are deliberately NOT the onboarding demo UUIDs, so a
// run never overwrites a human's local demo admins (cmd/useradmin upserts the
// password on conflict).

export const TENANT_A_ID = "33333333-3333-3333-3333-333333333333";
export const TENANT_B_ID = "44444444-4444-4444-4444-444444444444";

// Three users (maintainer decision #2): tenant A's admin requests, a SECOND
// user in the SAME tenant approves (self-approval is forbidden for everyone,
// admins included — docs/rbac.md, approval-eligibility.ts), and tenant B's
// admin is only the switch target. Emails are unique across tenants
// (identity fails logins that match two users — onboarding §5.1 note).
export const TENANT_A_ADMIN_EMAIL = "e2e-admin@acme.example";
export const TENANT_A_APPROVER_EMAIL = "e2e-approver@acme.example";
export const TENANT_B_ADMIN_EMAIL = "e2e-admin@glow.example";

// Shared fixture password for all three users; it doubles as the web app's
// DEMO_ADMIN_PASSWORD (the tenant switcher re-logs in with it —
// apps/web/src/app/api/auth/switch-tenant/route.ts reads DEMO_ADMIN_PASSWORD
// and degrades to a /login redirect without one).
export const E2E_ADMIN_PASSWORD = "e2e-happy-path-59";

// One active vendor per tenant (created via the gateway in globalSetup, the
// onboarding §5.1 pattern). Distinct codes make the tenant-isolation check
// read unambiguously on /app/vendors.
export const TENANT_A_VENDOR = {
  code: "E2E-ACME-01",
  name: "E2E Acme Supplies",
  contactEmail: "sales@e2e-acme.example",
  taxId: "01.333.333.3-033.333",
} as const;

export const TENANT_B_VENDOR = {
  code: "E2E-GLOW-01",
  name: "E2E Glow Supplies",
  contactEmail: "sales@e2e-glow.example",
  taxId: "02.444.444.4-044.444",
} as const;

// The single-step IDR approval chain installed per tenant A (onboarding
// §5.1): any total, decided by role "approver" (the second fixture user is
// admin, so the role check is exempt — the self-approval check is not).
export const APPROVAL_RULES_BODY = {
  rules: [{ currency: "IDR", step_no: 1, role_required: "approver", threshold_cents: 0 }],
};

// The requisition the spec drives through the wizard: one item, qty 2, so
// the PO line, the full GRN (qty 2), and the invoice (qty 2 at the same
// price) line up for a `matched` verdict.
export const REQUISITION_ITEM = {
  description: "E2E happy-path laptop",
  qty: "2",
  unitPriceCents: "7500000",
  category: "it",
} as const;

// Gateway base URL — the compose stack's published gateway port.
export const GATEWAY_URL = process.env.E2E_GATEWAY_URL || "http://localhost:8080";

// Web app base URL (Playwright's baseURL and the webServer probe).
export const WEB_URL = process.env.E2E_WEB_URL || "http://localhost:3000";
