# Dev note — #59 Playwright e2e: happy path

- **Issue:** #59 (part of #11, milestone M4)
- **Branch:** feature/59-playwright-e2e-happy-path
- **Date:** 2026-11-01

> **Provenance:** the two parked questions were answered on the issue
> (2026-10-31, maintainer override welcome in PR review): (1) the convert
> step calls the gateway endpoint directly from the spec — the missing UI is
> product scope tracked separately (issue #112); (2) #59 ships now with its
> own deterministic fixtures instead of waiting for `make seed` to write the
> live schema (#110) — the tested entities are created through the UI by the
> spec itself.

## What changed

- **`e2e/` (new workspace package, `@eprocurement/e2e`)** — the Playwright
  happy-path suite, ADR-0002's e2e stage (`Playwright against the compose
  stack`) and PRD §22's DoD ("Playwright e2e covers happy path"):
  - **`playwright.config.ts`** — one sequential story, `workers: 1`,
    `fullyParallel: false`, retries capped at 1 (CI only); `screenshot:
    "only-on-failure"` + `trace: "retain-on-failure"` (the "screenshots on
    failure" AC); a managed `webServer` running `pnpm --filter
    @eprocurement/web start` with the three demo-switcher env values
    (`GATEWAY_URL`, `DEMO_TENANTS`, `DEMO_ADMIN_PASSWORD`) injected from
    committed defaults unless the environment already provides them — the
    runner has no `apps/web/.env.local`, and without `DEMO_ADMIN_PASSWORD`
    the switcher degrades to a `/login` redirect
    (apps/web/src/app/api/auth/switch-tenant/route.ts:32-37) so the switch
    step could never pass.
  - **`support/fixtures.ts`** — the deterministic fixture constants: two
    e2e-owned tenants (`3333…`/`4444…` — deliberately NOT the onboarding
    demo UUIDs, so a run never rewrites a human's local demo admin), three
    users (tenant A admin = requester, a second tenant A user with the
    admin role = approver, tenant B admin = switch target), one vendor per
    tenant, and the single-line requisition the spec drives. The shared
    password is a fixture credential for throwaway stacks (the onboarding
    `demo-password` shape), not a secret.
  - **`support/fixtures.global-setup.ts`** — installs the fixtures the
    onboarding §5.1 way: three users via `go run
    ./services/identity/cmd/useradmin` (idempotent `ON CONFLICT` upsert),
    then per tenant one vendor + the IDR approval chain through the gateway
    (login → `POST /api/v1/vendors`, tolerating `409
    vendor_duplicate_code` on replays → `PUT /api/v1/approval-rules`).
  - **`support/gateway.ts`** — typed gateway REST helpers (health poll,
    login, vendor create/lookup, approval-rule install, convert-to-PO).
  - **`specs/happy-path.spec.ts`** — the story (below).
  - **`fixtures/invoice-attachment.png`** — a valid 1×1 PNG (68 bytes,
    verified PNG magic/IHDR/IEND). The upload path rejects anything outside
    the pdf/png/jpeg/webp/docx/xlsx allowlist client-side
    (`upload_mime_unsupported`, upload.ts:13-20/133) and `complete`
    re-verifies size/mime/checksum server-side — a `.txt` fixture would
    never reach `ready`, leaving the Match button permanently gated
    (InvoiceActions.tsx:48-53).
  - **`e2e/.gitignore`** — `test-results/` + `playwright-report/` stay out
    of git; the package defines NO `build`/`test` scripts so the turbo
    gates (`pnpm build` / `pnpm test`) never run Playwright without a
    stack — the suite runs via `pnpm --filter @eprocurement/e2e e2e`.
- **`.github/workflows/ci.yml`** — new `e2e` job (`needs: [go, web]`,
  `timeout-minutes: 45` — it builds all five service images with no layer
  cache): checkout → Go (`go-version-file`, for `go run
  ./services/identity/cmd/useradmin`) → pnpm + Node 22 →
  `pnpm install --frozen-lockfile` → `pnpm --filter @eprocurement/web
  build` → `playwright install --with-deps chromium` → `docker compose up
  -d --wait` → run the suite → upload `playwright-report` + `test-results`
  via `actions/upload-artifact@v4` with `if: failure() || cancelled()`
  (not `failure()` alone: concurrency `cancel-in-progress` is active at
  ci.yml:13-15, and a cancelled run must still carry its screenshots).
- **`apps/web/package.json`** — adds the standard `start: "next start"`
  script (the config's webServer uses it; no behavior change).
- **`pnpm-workspace.yaml` / `pnpm-lock.yaml`** — `e2e` joins the workspace
  with its own locked devDependencies (`@playwright/test` 1.64.1,
  `typescript`, `eslint`, `typescript-eslint`, `@types/node`).
- **`Makefile`** — `make e2e`: build web, then run the suite with
  `DATABASE_URL` passed from `.env` (the Makefile's existing sed pattern)
  so a locally overridden `POSTGRES_PORT` (e.g. 55432) still lets
  useradmin reach the compose database.

## The covered story (in order)

1. **Login** as tenant A's admin through `/login`; the banner shows the
   session's tenant id.
2. **Tenant switch + isolation**: on `/app/vendors` the tenant A vendor is
   visible; switching through the TenantSwitcher (`#tenant-switcher` →
   `glow`) reloads as tenant B's admin — the glow vendor appears, the acme
   vendor disappears, and the banner shows tenant B's UUID; switching back
   restores tenant A's view (#50's acceptance shape).
3. **PR create + submit** through the RequisitionWizard's four steps
   (currency IDR → one line "E2E happy-path laptop" ×2 @ 7,500,000 cents →
   no PR attachment → review total 15,000,000 → submit → `PR-%06d`
   confirmation).
4. **Self-approval is forbidden even for admins** — asserted both ways:
   the requester's own detail view renders NO approval actions
   (`approval-eligibility.ts:77-79` nulls the decidable step), and the
   approve is performed by a **second user of the same tenant** after
   logout + fresh `/login` (the server twin: `ErrSelfForbidden`,
   approval.go; `approval_self_forbidden`, api_test.go).
5. **Convert to PO** — direct gateway call from the spec
   (`POST /api/v1/purchase-orders/from-requisition/:id` with the
   `vendor_id` resolved by code; po:write held by the admin approver) per
   the maintainer decision; the UI gap is issue #112.
6. **PO lifecycle** — draft → `po-issue` → issued → `po-acknowledge` →
   acknowledged (GRN requires acknowledged/partially_received,
   po-eligibility.ts:50-52) → **full goods receipt** through
   GoodsReceiptForm (each line's `max` attribute IS the remaining
   quantity) → PO `received`.
7. **Invoice + match** — `po-create-invoice-link` → the invoice form
   (number unique per run, qty = the received 2, PO price prefilled) →
   detail page `pending` → upload the PNG through the panel
   (declare → presigned PUT → complete) until the attachment row shows
   `ready` and the Match gate disappears → click Match →
   `match-result-panel` visible, `match-verdict` = "Matched", invoice
   status `matched`.

## Decisions

- **Own fixtures, not `make seed`** (maintainer decision #2): the seed
  writes only the isolated `demo.*` schema no service reads (#110 will
  change that); the spec instead installs three users, two vendors, and
  the approval chain in the real schema (the onboarding §5.1 pattern) and
  creates every tested entity (PR/PO/GRN/invoice) itself. All fixture
  writes are idempotent (useradmin upsert, vendor-create 409-tolerant,
  approval-rules PUT), so re-runs and Playwright retries are safe.
- **Convert via the gateway API, not a UI slice** (maintainer decision
  #1): smallest coherent change; #112 tracks the missing UI.
- **A second same-tenant user approves** — an admin requester cannot
  approve their own requisition (the #30 rule, `docs/rbac.md`); giving the
  second user the admin role keeps the remaining flow (po:write,
  grn:post, invoice:write) in one login — admin is exempt from the rule's
  `role_required` but never from the self-approval gate.
- **Rate-limit mitigation is explicit** (the onboarding §5.1 trap): the
  CI job sets `IDENTITY_LOGIN_RATE_PER_MIN=60` in its environment before
  `docker compose up` — compose interpolates it
  (compose.yaml `${IDENTITY_LOGIN_RATE_PER_MIN:-5}`), so no compose.yaml
  change. Login budget per email per run: acme 3 (setup + UI + switch
  back), approver 2 (UI + API), glow 2 (setup + switch); one retry worst
  case doubles those (6/4/4) — under identity's raised 60/min and under
  the gateway's unwired 30/min per-IP default (12 total). Locally the
  identity default of 5/min per email is the binding constraint: single
  runs fit (max 3/email), back-to-back runs within a minute trip it —
  observed live during verification (the switch-back answers
  `switch_failed` and redirects to `/login`); spacing runs a minute apart
  or setting `IDENTITY_LOGIN_RATE_PER_MIN` locally avoids it.
- **`retries: process.env.CI ? 1 : 0`** — capped at 1 so a retried story
  stays inside the login budgets; a local failure keeps its single,
  screenshot-rich attempt.
- **The web app is exercised through the production server** (`next
  start` on the web build), not `next dev` — the demo artifact.
- **Locale pinned via the `eproc_locale` cookie ("en")** — the app's
  DEFAULT_LOCALE is "id" (i18n/index.ts); pinning makes the EN copy
  assertions (button names, "Matched") deterministic.
- **No `test`/`build` scripts in the e2e package** — golden rule 7's
  gates stay unchanged: `pnpm test` remains Go + web unit suites only;
  Playwright needs a live stack and never runs implicitly.

## Evidence

All commands run from the repo root against the local compose stack
(PostgreSQL published on the local override `:55432` via `.env`; every
service healthy per `docker compose ps`):

- `pnpm --filter @eprocurement/e2e exec tsc --noEmit` — clean.
- `pnpm --filter @eprocurement/e2e lint` — clean (part of `pnpm lint`).
- `docker compose config -q` — clean (no compose.yaml change; the rate
  override rides the job environment).
- `DATABASE_URL=postgres://…@localhost:55432/… pnpm --filter
  @eprocurement/e2e e2e` — **1 passed (5.6s wall, 2.9s test)**: the full
  story — login, tenant switch with isolation asserts, 4-step wizard
  submit, requester action-less, second-user approve, gateway convert
  (PO-000001), issue → acknowledge → full GRN (status `received`),
  invoice pending → PNG attachment `ready` → Match → verdict "Matched".
- Re-run after spacing (rate window) — **1 passed (2.9s)**: fixtures are
  idempotent; a second requisition/PO/invoice stack on top of the first.
- Back-to-back run without spacing — fails at the switch-back with
  `switch_failed` (identity's 5/min per-email limiter; 6 acme logins < 2
  min) — the documented trap; the Playwright screenshot + trace captured
  it, demonstrating the failure artifact path. CI sets the limit to 60.
- `make e2e` — same green result through the Makefile target (web rebuild
  + suite).
- `go vet ./... && go build ./...` — clean. `go test ./...` — 34
  packages ok (no Go files touched).
- `pnpm install --frozen-lockfile` (implicit via `pnpm install` on the
  updated lockfile), `pnpm build` (3/3), `pnpm lint` (4/4, now including
  `@eprocurement/e2e`), `pnpm test` (6/6) — all green.
- `python -c "yaml.safe_load(.github/workflows/ci.yml)"` — jobs
  `[go, web, e2e]`, `e2e.needs == [go, web]`, `timeout-minutes == 45`.
- **Not run here:** the GitHub Actions job itself — first execution
  happens on the PR push (no local runner available); the job mirrors the
  verified local sequence plus `docker compose up -d --wait` on a fresh
  runner.

## Follow-ups

- **#112** — the convert-to-PO UI this spec bypasses via the gateway; when
  it lands, the spec's step 5 should switch to driving it.
- **#60** — the resilience e2e scenario (worker kill/replay) on this same
  harness; PRD §23 wants "happy path + one resilience scenario".
- **#110** — when the seed loads the live schema, revisit whether the e2e
  fixtures should derive from it instead of self-installing.
- **Login budget note for local runs** — if `make e2e` is run in a loop,
  set `IDENTITY_LOGIN_RATE_PER_MIN` in `.env` and `make compose-up` (or
  space runs a minute); CI already raises it.
- **Docker layer cache for the e2e job** — the 45-minute budget covers
  five uncached image builds; if CI wall time becomes a problem,
  `docker/build-push-action` with GHA cache is the lever (an
  infrastructure change under golden rule 10, so not snuck in here).
