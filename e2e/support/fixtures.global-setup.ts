// Global setup for the #59 happy-path e2e: install the suite's deterministic
// fixtures (maintainer decision #2 on issue #59 — the spec owns its data,
// because `make seed` only writes the isolated demo.* schema no service
// reads). This mirrors the one-time setup in docs/onboarding.md §5.1:
//
//   1. three users via cmd/useradmin (idempotent ON CONFLICT upsert):
//      tenant A admin (requester), a SECOND tenant A user with the admin
//      role (approver — self-approval is forbidden for everyone, admins
//      included, so a distinct user must decide), and tenant B's admin
//      (switch target);
//   2. per tenant: one active vendor through the gateway (tolerating the
//      vendor_duplicate_code replay of a previous run);
//   3. tenant A: the single-step IDR approval chain (PUT approval-rules).
//
// The tested entities (requisition, PO, GRN, invoice) are NOT seeded — the
// spec creates them through the UI itself.
import { execFile } from "node:child_process";
import { resolve } from "node:path";
import { promisify } from "node:util";

import {
  APPROVAL_RULES_BODY,
  E2E_ADMIN_PASSWORD,
  TENANT_A_ADMIN_EMAIL,
  TENANT_A_APPROVER_EMAIL,
  TENANT_A_ID,
  TENANT_A_VENDOR,
  TENANT_B_ADMIN_EMAIL,
  TENANT_B_ID,
  TENANT_B_VENDOR,
} from "./fixtures";
import { ensureVendor, gatewayLogin, installApprovalRules, waitForGateway } from "./gateway";

const run = promisify(execFile);

// Playwright transpiles the support files to CommonJS (no import.meta
// shim in its loader), so __dirname is the portable way to the repo root.
const repoRoot = resolve(__dirname, "..", "..");

// The identity default mirrors services/identity/config (localhost:5432,
// the compose credentials); DATABASE_URL from the environment wins so a
// locally overridden POSTGRES_PORT keeps working (Makefile passes it; an
// EMPTY value — `make e2e` without a .env — falls back, hence `||`).
const DATABASE_URL =
  process.env.DATABASE_URL || "postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable";

async function createUser(email: string, tenantId: string, name: string, roles: string[]): Promise<void> {
  const args = [
    "run",
    "./services/identity/cmd/useradmin",
    "-email",
    email,
    "-password",
    E2E_ADMIN_PASSWORD,
    "-tenant-id",
    tenantId,
    "-name",
    name,
    ...roles.flatMap((role) => ["-role", role]),
  ];
  const { stdout } = await run("go", args, {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL },
  });
  console.log(`[e2e setup] ${stdout.trim()}`);
}

export default async function globalSetup(): Promise<void> {
  await waitForGateway();

  await createUser(TENANT_A_ADMIN_EMAIL, TENANT_A_ID, "E2E Acme Admin", ["admin"]);
  await createUser(TENANT_A_APPROVER_EMAIL, TENANT_A_ID, "E2E Acme Approver", ["admin"]);
  await createUser(TENANT_B_ADMIN_EMAIL, TENANT_B_ID, "E2E Glow Admin", ["admin"]);

  const tokenA = await gatewayLogin(TENANT_A_ADMIN_EMAIL, E2E_ADMIN_PASSWORD);
  await ensureVendor(tokenA, TENANT_A_VENDOR);
  await installApprovalRules(tokenA, APPROVAL_RULES_BODY);

  const tokenB = await gatewayLogin(TENANT_B_ADMIN_EMAIL, E2E_ADMIN_PASSWORD);
  await ensureVendor(tokenB, TENANT_B_VENDOR);

  console.log("[e2e setup] fixtures ready (users, vendors, approval rules)");
}
