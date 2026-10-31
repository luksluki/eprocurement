import { defineConfig } from "@playwright/test";

import {
  E2E_ADMIN_PASSWORD,
  GATEWAY_URL,
  TENANT_A_ADMIN_EMAIL,
  TENANT_B_ADMIN_EMAIL,
  WEB_URL,
} from "./support/fixtures";

// The happy-path spec runs as ONE sequential story against ONE compose
// stack: a single worker, no parallelism, retries capped at 1 (each retry
// replays the whole login sequence, which stays well inside the CI login
// budgets — see docs/dev-notes/59-playwright-e2e-happy-path.md).
export default defineConfig({
  testDir: "./specs",
  globalSetup: "./support/fixtures.global-setup.ts",
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  forbidOnly: !!process.env.CI,
  timeout: 180_000,
  expect: { timeout: 15_000 },
  outputDir: "./test-results",
  reporter: [
    ["list"],
    ["html", { outputFolder: "playwright-report", open: "never" }],
  ],
  // AC "screenshots on failure": screenshot on failure + trace retained on
  // failure; the CI job uploads both report folders as artifacts
  // (also on cancelled runs — concurrency cancel-in-progress is active).
  use: {
    baseURL: WEB_URL,
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  // `next start` against the compose gateway. The three demo-switcher env
  // values are the documented apps/web/.env.example set: without
  // DEMO_ADMIN_PASSWORD the switcher degrades to a /login redirect and the
  // tenant-switch step cannot work. Explicit env values win over these
  // defaults (the CI job sets all three itself).
  webServer: {
    command: "pnpm --filter @eprocurement/web start",
    url: WEB_URL,
    timeout: 180_000,
    reuseExistingServer: !process.env.CI,
    stdout: "pipe",
    stderr: "pipe",
    env: {
      ...process.env,
      GATEWAY_URL: process.env.GATEWAY_URL || GATEWAY_URL,
      DEMO_TENANTS:
        process.env.DEMO_TENANTS || `acme=${TENANT_A_ADMIN_EMAIL},glow=${TENANT_B_ADMIN_EMAIL}`,
      DEMO_ADMIN_PASSWORD: process.env.DEMO_ADMIN_PASSWORD || E2E_ADMIN_PASSWORD,
    },
  },
});
