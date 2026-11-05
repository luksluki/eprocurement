import { defineConfig } from "@playwright/test";

import base from "./playwright.config";

// Demo-capture profile (#64): same specs and stack as CI, rendered at 1080p
// with video on and a slower interaction pace so the recording is watchable.
// Not used by CI. The health-check URL is /login because the app root
// answers 404 by design.
const webServer = {
  ...(base.webServer as Record<string, unknown>),
  url: "http://127.0.0.1:3000/login",
  command: "pnpm --filter @eprocurement/web exec next start -H 0.0.0.0 -p 3000",
};

export default defineConfig({
  ...base,
  webServer,
  outputDir: "./test-results-video",
  use: {
    ...(base.use as Record<string, unknown>),
    viewport: { width: 1920, height: 1080 },
    video: { mode: "on", size: { width: 1920, height: 1080 } },
    launchOptions: { slowMo: 3800 },
  },
});