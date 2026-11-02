# Dev note — #62 README with capability matrix

- **Issue:** #62 (part of #12, milestone M5)
- **Branch:** feature/62-readme-capability-matrix
- **Date:** 2026-11-02

## What changed

- **`README.md`** — rewritten from a "planning" landing page into a
  reviewer-facing map of the implemented system (docs-only diff; no code,
  compose, Makefile, or CI changes):
  - **Status line** — replaced the stale `Status: planning` with the real
    state: the procure-to-pay slice is implemented and CI-gated; M5 demo
    artifacts (#63/#64/#65) and the k8s kind manifests (D4) are open.
  - **Architecture (implemented)** — replaced the `(target)` sketch with the
    topology `make compose-up` actually starts (ADR-0001 + `compose.yaml`):
    web → gateway → identity/procurement/documents over gRPC, the
    transactional-outbox → RabbitMQ → worker flow into the documents
    projection, Redis as the export lock/heartbeat store, MinIO for
    attachments + exports, the OTel collector in the default stack, and the
    `observability` profile (Jaeger/Prometheus/Grafana) drawn as opt-in.
  - **Repository layout** — replaced the `(target)` layout with the verified
    tree (`internal/platform`, `tests/integration`, `deploy/{compose,docker}`,
    `e2e/`, …) and an explicit note that `deploy/k8s` is NOT in the repo yet.
  - **Quickstart** — the reviewer path is now `cp .env.example .env` →
    `make install` → `make compose-up` → `make seed`, with the compose-up
    comment listing all **11 default services** (the old comment omitted
    `documents` and `worker`); worker's "no published ports" reality added to
    the endpoints table; new **"See the web UI"** section condensing
    onboarding §5.1 (useradmin, vendor + approval-rule curl, `apps/web/.env.local`
    with `DEMO_TENANTS`/`DEMO_ADMIN_PASSWORD`, `pnpm dev`) and a **make-targets
    table**.
  - **Capability-to-evidence matrix** (the PRD §23 deliverable) — two tables
    covering all **15 rows of PRD §20** and all **12 MVP in-scope items of
    PRD §10**, each mapped to code paths verified in this session plus a
    test/CI/doc evidence column. Pending capabilities are labeled with their
    tracking issue, never claimed.
  - **Demo & review artifacts** — links #63 (script), #64 (video), #65 (tunnel
    runbook) as open issues with ADR-0003 for hosting context.
  - **CI section** — updated from "two jobs" to the four real jobs
    (go / web / integration / e2e) per `.github/workflows/ci.yml`.
  - **Docs section** — ADR-0001..0003 linked explicitly.
- **`docs/dev-notes/62-readme-capability-matrix.md`** — this note.

## Decisions

- **Honest-pending over claiming (PRD §3 "not from claims").** Five gaps are
  stated in the matrix instead of papered over: (1) `deploy/k8s` kind
  manifests (D4) not in the repo; (2) Playwright resilience scenario = open
  #60; (3) Redis read-model cache layer (PRD §13/§20 "cache") not
  implemented — only the export lock/heartbeat; (4) Zustand from PRD §20's
  web row is not a dependency (client state lives in TanStack Query);
  (5) requisition *cancel* exists only as a domain transition — no REST/UI
  surface. Notifications are half-real: the Mongo projection + worker
  consumer exist (#47), the in-app UI surfacing and SMTP stub do not.
- **Demo artifacts are links, not content.** #63/#64/#65 are open issues of
  demo epic #12; the README links them as the plan and points at `make e2e`
  (+ `make test-p2p`) as the reproducible stand-in for the PRD §22 story
  until they land.
- **PRD §20's `docs/mongo.md` does not exist** — the Mongo schema/pipeline
  documentation actually lives in `docs/schema/documents.md`; the matrix
  cites the real path.
- **GitLab CI row mapped via ADR-0002**, not faked: the README says the four
  GitHub Actions jobs implement ADR-0002's stages and that a literal
  `.gitlab-ci.yml` is intentionally absent (ADR-0002's own negative
  consequence note).
- **Quickstart keeps `make seed` even though the seed writes only the
  `demo` schema** (#110 open): it is still the reviewer's one-command data
  step for perf/scale, and the new "See the web UI" section explains the
  §5.1 app-schema setup — the acceptance criterion asks for exactly that
  split to be visible.
- **Architecture diagram stays ASCII** (consistent with every other diagram
  in the repo's docs) and encodes the compose reality: OTel collector
  default, Jaeger/Prometheus/Grafana behind `--profile observability`.

## Evidence

Docs-only diff; the checks are existence/accuracy checks of every claim the
new README makes (run from the repo root, this session):

- Required reading done: `AGENTS.md`, `docs/prd/prd-v0.2.md`,
  `docs/adr/0001-architecture.md`, `docs/adr/0002-ci-github-actions.md`,
  `docs/adr/0003-demo-hosting.md`, `docs/backlog-v0.1.md`,
  `docs/onboarding.md`, `docs/dev-notes/59-playwright-e2e-happy-path.md`,
  `.github/workflows/ci.yml`, `Makefile`, `compose.yaml`.
- Quickstart commands exist: `.env.example` (ls), `install`/`compose-up`/
  `seed` (Makefile:26/44/59).
- Default compose services counted from `compose.yaml`: postgres, mongo,
  redis, rabbitmq, minio, otel-collector, identity, procurement, documents,
  worker, gateway = **11** (prometheus/grafana/jaeger carry
  `profiles: [observability]`, compose.yaml:108/127/155).
- Matrix code paths verified by listing/grep: `services/{gateway,identity,
  procurement,documents,worker}` package layouts; `internal/platform`
  (telemetry, metrics, rbac, jwt, tenant, dbmigrate, ratelimit, …);
  `proto/{identity,procurement,documents}/v1` with `grep -c "rpc "` =
  4/29/9; migrations 3 + 15 (`find services -name "*.sql"`); gateway REST
  paths extracted from `services/gateway/server/*.go` (auth, vendors,
  requisitions+attachments+submit, approvals, approval-rules,
  purchase-orders+issue+goods-receipts, invoices+match+attachments,
  match-settings, dashboard, events, audit+export, exports+runs, dlq+replay);
  requisition cancel domain-only (`requisition.go:264`, no proto RPC /
  gateway route); Redis hits only in worker export + telemetry hook (grep
  -il redis); `zustand` absent from `apps/web/package.json` and src (grep);
  TanStack/RHF/Zod/Tailwind present in `apps/web/package.json`;
  `pnpm-workspace.yaml` + `turbo.json` exist; web unit test files = 51
  (`find apps/web/src -name "*.test.*" | wc -l`); `e2e/specs/
  happy-path.spec.ts` + `tests/integration/p2p_{flow,failure}_test.go`
  exist; `deploy/k8s` absent (`ls deploy`); `docs/dev-notes/` holds 48 files
  including this note (47 before this branch).
- Linked issues confirmed OPEN via `gh issue view`: #63, #64, #65, #60, #110.
- Link check: every repo-relative path referenced in the new README exists
  (scripted check, 0 missing).
- Gates: go vet/build/test and pnpm install/build/lint/test are **not run
  here** — the diff touches only `README.md` and this note (no Go/TS
  sources), and the orchestrating script runs the full gate set after this
  step. Markdown rendered tables checked for consistent column counts.

## Follow-ups

- **#63/#64/#65** — when the demo script/video/runbook land, replace the
  issue links with the artifacts; the "See the web UI" section should then
  point at the scripted path instead of §5.1 curl snippets.
- **#110** — when the seed loads the live schema, fold the §5.1 manual setup
  into `make seed` and shrink the quickstart's web-UI step.
- **D4 (k8s)** — add `deploy/k8s` + the CI kubeconform stage (ADR-0002
  stage 6), then drop the "pending" markers from the matrix and the layout
  note.
- **#60/#112** — resilience e2e and the convert-to-PO UI; both rows in the
  matrix get their pending notes removed when they land.
- **#62 PR review** — the maintainer may want the PRD §20 row for Zustand
  resolved either way (adopt it or amend the PRD row) rather than carrying
  the "not a dependency" note indefinitely.
