# eProcurement — ELCODE ProcureKit

[![CI](https://github.com/luksluki/eprocurement/actions/workflows/ci.yml/badge.svg)](https://github.com/luksluki/eprocurement/actions/workflows/ci.yml)

Multi-tenant eProcurement platform (procure-to-pay slice) and reference implementation: **Go microservices + Next.js**, PostgreSQL + MongoDB + Redis + RabbitMQ + MinIO, OpenTelemetry, Docker Compose.

**Status:** the procure-to-pay slice is **implemented and running** — PR → approval → PO → GRN → 3-way invoice match, event history, exports, observability, and a Playwright e2e suite (happy path + worker-crash resilience), all gated in CI. Milestone M5 demo artifacts (the demo video #64) and the Kubernetes kind manifests (D4) are still open — see [Demo & review artifacts](#demo--review-artifacts) and the [capability matrix](#capability-to-evidence-matrix) for exactly what is real and what is pending. PRD: [`docs/prd/prd-v0.2.md`](docs/prd/prd-v0.2.md).

## Architecture (implemented)

Consistent with [ADR-0001](docs/adr/0001-architecture.md) and [`compose.yaml`](compose.yaml) — this is the topology `make compose-up` actually starts:

```text
                ┌────────────────────────────────┐
                │  apps/web (Next.js 15)         │
                │  TanStack Query/Table · RHF/Zod│
                │  Tailwind · i18n (id/en)       │
                └───────────────┬────────────────┘
                                │ REST (JWT; BFF route handlers)
                ┌───────────────▼────────────────┐
                │  gateway (Go · GoFiber)        │
                │  JWT verify · RBAC · rate      │
                │  limits · DLQ admin endpoint   │
                └──┬──────────┬──────────┬───────┘
                   │ gRPC     │ gRPC     │ gRPC
         ┌─────────▼───┐ ┌────▼────────┐ ┌▼─────────────────┐
         │ identity    │ │ procurement │ │ documents        │
         │ users/roles │ │ vendors PR/ │ │ event history ·  │
         │ login/JWKS  │ │ approval PO/│ │ audit · notifs · │
         │             │ │ GRN invoice │ │ attachment meta  │
         │ PostgreSQL  │ │ PG + outbox │ │ MongoDB · MinIO  │
         └─────────────┘ └────┬────────┘ └────────▲──────────┘
                             writes state + event │ consume (history,
                             in ONE transaction   │  notifications)
                                  outbox table    │
                                  │  publish      │
                  ┌───────────────▼───────────────┴──────┐
                  │ RabbitMQ · exchange                  │
                  │ `procurement.events` · DLX · retry   │
                  └───────────────┬──────────────────────┘
                                  │
                        ┌─────────▼──────────┐
                        │ worker (Go)        │
                        │ outbox publisher · │
                        │ idempotent         │
                        │ consumers · cron   │
                        │ exports (CSV/MinIO)│
                        └────────────────────┘

  Redis — export cron lock + heartbeat        MinIO — attachments + export CSVs
  OTel collector (default stack) ← OTLP spans from all five Go services
  Profile `observability` (opt-in): Jaeger UI (traces) · Prometheus (scrapes the
  services' container-internal /metrics ports 9100–9104) · Grafana (dashboard)
```

Synchronous calls are REST at the edge and gRPC internally; `procurement` is the system of record, `documents` is an append-only projection (CQRS-lite). Killing the worker loses nothing — the outbox retains events and consumers dedupe by `event_id` (ADR-0001, [PRD §14](docs/prd/prd-v0.2.md)).

## Repository layout

```text
apps/web/                    Next.js App Router + TypeScript (UI + BFF route handlers)
services/gateway/            Go · REST edge/BFF (GoFiber)
services/identity/           Go · users, roles, login, refresh, JWKS (PostgreSQL)
services/procurement/        Go · vendors, PR/approval/PO/GRN/invoice (PostgreSQL + outbox)
services/documents/          Go · event history, audit, notifications, attachments (MongoDB + MinIO)
services/worker/             Go · outbox publisher, RabbitMQ consumers, cron exports, DLQ monitor
internal/platform/           shared Go: telemetry, metrics, RBAC, JWT, tenant ctx, migrations, rate limit
packages/{ui,config}/        shared React components + tooling presets (pnpm workspace + Turborepo)
proto/ · gen/go/             gRPC contracts (identity/procurement/documents v1) + generated stubs
deploy/compose/              stack config: OTel collector, Prometheus, Grafana provisioning
deploy/docker/               one Dockerfile, five service images
e2e/                         Playwright e2e: happy path + worker-crash replay (@eprocurement/e2e)
tests/integration/           procure-to-pay cross-service suite (make test-p2p)
scripts/                     seed, dev gates, grpc-health-probe
docs/                        PRD, ADRs, schemas, RBAC, perf, onboarding, dev notes
```

> `deploy/k8s` (kind manifests, decision D4) is **not yet in the repo** — tracked as pending, not claimed.

## Quickstart

Prerequisites: Go 1.24+, Node 22+, pnpm, GNU Make, and Docker Desktop (or Docker Engine with Compose v2). Full walkthrough with expected output and troubleshooting: [`docs/onboarding.md`](docs/onboarding.md).

```bash
cp .env.example .env   # local-only credentials; defaults work out of the box
make install           # pnpm install
make compose-up        # docker compose up -d --wait, blocks until healthy:
                       #   postgres · mongo · redis · rabbitmq · minio · otel-collector
                       #   identity · procurement · documents · worker · gateway   (11 services)
make seed              # deterministic demo dataset (3 tenants, 100k requisitions,
                       # 1M events) — budget-gated under 2 minutes
```

First `compose-up` builds the five Go service images (~2–3 min); later runs take seconds. Then open the [web UI](#see-the-web-ui) or run the gates: `make lint && make test` (what CI runs).

| Service | Endpoint |
|---|---|
| gateway (REST) | http://localhost:8080 (`/healthz`, `/.well-known/jwks.json`) |
| identity (gRPC + health) | `localhost:8081` |
| procurement (gRPC + health) | `localhost:8082` |
| documents (gRPC + health) | `localhost:8083` — attachments: MongoDB `document_meta` + MinIO presigned uploads |
| worker | no published ports — dials out only; container-internal `/metrics` on 9104 |
| PostgreSQL | `localhost:5432` |
| MongoDB | `localhost:27017` |
| Redis | `localhost:6379` |
| RabbitMQ (AMQP / UI) | `localhost:5672` / http://localhost:15672 |
| MinIO (S3 / Console) | http://localhost:9000 / http://localhost:9001 |
| OTel collector (gRPC / HTTP / health) | `localhost:4317` / `localhost:4318` / http://localhost:13133 |
| Prometheus (observability profile) | http://localhost:9090 — scrapes the services' container-internal `/metrics` ports (9100–9104) |
| Grafana (observability profile) | http://localhost:3001 — admin/`GRAFANA_ADMIN_PASSWORD`, dashboard auto-provisioned |
| Jaeger UI (observability profile) | http://localhost:16686 — one PO request flow as ONE trace: gateway → gRPC → pgx → outbox publish → consume |

Run `docker compose --profile observability up -d` to add Prometheus, Grafana, and the Jaeger trace UI; the default stack stays lean (the profile deviation from PRD §14.9 is recorded in [`docs/dev-notes/055-prometheus-metrics-grafana-dashboard.md`](docs/dev-notes/055-prometheus-metrics-grafana-dashboard.md)).

Reset the local stack (drops volumes): `make reset`.

> Port conflict? If a service port is already used on your machine (for example a local PostgreSQL on 5432), override it in `.env` — e.g. `POSTGRES_PORT=55432` — and update the matching connection string (see [onboarding §3](docs/onboarding.md)).

### See the web UI

`make seed` writes the isolated `demo` schema only — `identity.users` starts empty, so the web shell needs a one-time setup (full copy-paste commands: [onboarding §5.1](docs/onboarding.md#51-demo-tenant-admins-the-web-shell-and-tenant-scoped-app-data)):

1. Create a tenant admin with `go run ./services/identity/cmd/useradmin -email admin@acme.example -password '…' -tenant-id 11111111-1111-1111-1111-111111111111 -role admin -name "Acme Admin"`.
2. Log in once and create a vendor plus the approval rule chain through the gateway (`POST /api/v1/vendors`, `PUT /api/v1/approval-rules`) — curl snippets in onboarding §5.1.
3. `cp apps/web/.env.example apps/web/.env.local` and set `DEMO_TENANTS=acme=admin@acme.example` and `DEMO_ADMIN_PASSWORD=<the admin password>`.
4. `pnpm dev` → http://localhost:3000 — login, requisition wizard, approvals, PO/GRN, invoices with 3-way match, dashboard, reports, audit, and the tenant switcher.

The whole flow is also driven end-to-end by the Playwright suite: `make e2e` (compose stack must be up) — the happy-path story plus the worker-crash/replay resilience scenario (PRD §22 step 6).

### Make targets

| Target | What it does |
|---|---|
| `make install` | `pnpm install` |
| `make build` | `go build ./...` + `pnpm build` |
| `make lint` / `make test` | `go vet` + `pnpm lint` / `go test` + `pnpm test` (CI gates) |
| `make test-unit` | unit-only Go suite with a duration gate (default 120s) |
| `make test-integration` | full `go test ./...` against the compose database/object store |
| `make test-p2p` | procure-to-pay cross-service suite: gRPC → outbox → RabbitMQ → Mongo projection |
| `make e2e` | Playwright e2e suite against the compose stack: happy path + worker-crash replay (#59/#60) |
| `make demo` / `demo-reset` | seeded 5-minute demo story: reset demo tenant + install fixtures / wipe only ([runbook](docs/demo-script.md)) |
| `make cover` / `make cover-procurement` | identity / procurement coverage vs 80% thresholds (stack up) |
| `make perf-budget` | 100k-scale fixtures, per-shape p95 vs budget report (stack up) |
| `make loadtest` | k6 mixed read/write load test against the compose stack (10 VU × 2m default; needs k6) — results & runbook: [docs/perf.md §9](docs/perf.md) (#61) |
| `make compose-up` / `compose-down` / `compose-logs` / `compose-ps` | stack lifecycle |
| `make seed` / `seed-reset` / `reset` | demo data: regenerate / wipe / drop volumes + recreate |
| `make gen-proto` | regenerate `gen/go` from `proto/` (needs protoc) |

## Demo data

`make seed` wipes and regenerates a deterministic dataset: 3 tenants, users, vendors, **100,000 requisitions**, and **1,000,000 domain events** (PostgreSQL `demo` schema + MongoDB `events`). It is budget-gated to complete in under two minutes (`SEED_MAX_SECONDS`).

```bash
make seed          # reset + generate (deterministic, seed=42)
make seed-reset    # wipe demo data only
make reset         # drop compose volumes and recreate the stack
go run ./scripts/seed --tenants 2 --requisitions 10000 --events 100000   # lighter run
```

The seed data feeds the scale/perf fixtures (`make perf-budget`, `make test-integration`); the app-schema demo users/vendors for the UI are the §5.1 setup above (loading the seed dataset into the live schema is tracked in #110).

For the 5-minute walkthrough, `make demo` resets a dedicated demo tenant and installs the seeded story (users, vendor, approval rules, tolerances) — the presenter runbook is [`docs/demo-script.md`](docs/demo-script.md).

## Capability-to-evidence matrix

Every must-have from [PRD §20](docs/prd/prd-v0.2.md) (technology stack) and [PRD §10](docs/prd/prd-v0.2.md) (MVP scope), mapped to verified code paths and the tests/CI/docs that prove them. PRD §23 asks the README to map every key capability to code/tests/demo steps — this is that map. Pending items are labeled honestly with their tracking issue.

### PRD §20 — Technology Stack

| Capability | Where it lives (verified) | Evidence: tests / CI / docs |
|---|---|---|
| Go microservices, clean/hexagonal | 5 services under `services/` (domain packages + `server` adapter + `cmd`), shared `internal/platform` | CI **Go** job (`go vet`/`build`/`test`); dev notes 019–059 |
| gRPC / Protobuf | `proto/{identity,procurement,documents}/v1/*.proto` — 4 + 29 + 9 RPCs; generated stubs in `gen/go/` | `go build ./...`; `make gen-proto`; `make test-p2p` drives the real gRPC API |
| REST (GoFiber) | `services/gateway/server/` — auth, vendors, requisitions (+attachments, submit), approvals, approval-rules, purchase-orders (+issue/GRN), invoices (+match, attachments), match-settings, dashboard, events, audit, exports, dlq | per-route `*_test.go`; CI **E2E** job drives the REST surface |
| PostgreSQL (indexes, EXPLAIN, N+1) | `services/identity/migrations/` (3, incl. tenant RLS), `services/procurement/migrations/` (15, incl. list/read indexes 0002, 0011, 0012, 0014, 0015) | `make perf-budget` (#56); EXPLAIN notes in `docs/perf.md`; migrations run in CI **Go** job |
| MongoDB (schema, aggregation, pagination) | `services/documents/{history,notifications,export,attachment}` — projections, filters, pagination | documents store tests + `make test-p2p` (Mongo projection asserted); schema in `docs/schema/documents.md` |
| Redis (cache, TTL, lock) | **Implemented:** export cron lock + heartbeat + missed-run age metric (`services/worker/export/lock.go`, `age_monitor.go`); OTel span hook (`internal/platform/telemetry/redis.go`). **Pending:** the read-model cache layer (PRD §13) is not implemented | #49 dev note; export tests; compose `REDIS_URL` |
| RabbitMQ (exchange, DLQ, retry, outbox) | transactional outbox `services/procurement/outbox` + publisher `services/worker/publisher`; topology with DLX + TTL retry queues `services/worker/topology/topology.go`; idempotent consumers `services/worker/consumer`; DLQ monitor + admin replay `services/worker/dlqmonitor`, `services/gateway/server/dlq.go` | `make test-p2p` incl. failure/retry path; CI **Integration** job; dev notes 045–048 |
| Cron + failed-run monitoring | nightly export scheduler (`services/worker/export/{scheduler,schedule}.go`), Redis lock, heartbeat key, `export_age` alert; manual trigger (`manual.go`, #53) | #49/#53 dev notes; `EXPORT_*` env in compose; Prometheus alert rule `deploy/compose/prometheus/alerts.yml` |
| React/Next + TanStack Query/Table + RHF/Zod | `apps/web/src/app/**` — login, dashboard, requisitions (list/wizard/detail), approvals, purchase-orders, vendors, invoices, reports, audit, admin/users, admin/settings; deps in `apps/web/package.json` | 51 web unit test files (`pnpm test`); CI **Web** job; CI **E2E**. Note: Zustand (PRD §20) is not currently a dependency — client state lives in TanStack Query |
| Monorepo (pnpm workspace + Turborepo) | `pnpm-workspace.yaml`, `turbo.json`, `apps/web`, `packages/{ui,config}`, `e2e` | `pnpm build`/`lint`/`test` run through turbo in CI **Web** job |
| OpenTelemetry + Prometheus | `internal/platform/telemetry` — spans REST → gRPC → pgx → outbox publish → RabbitMQ consume → Redis; `internal/platform/metrics` + `/metrics` ports 9100–9104; OTel collector in the default stack | #54/#55 dev notes; Jaeger/Prometheus/Grafana via `--profile observability`; gateway `trace_test.go` |
| Docker + Kubernetes | **Implemented:** `compose.yaml` (11 default services) + `deploy/docker/service.Dockerfile`; CI **E2E** builds all five images. **Pending:** `deploy/k8s` kind manifests (D4) not yet in the repo | `make compose-up`; CI **E2E** job |
| CI (GitLab-CI-shaped) | `.github/workflows/ci.yml` — 4 jobs mapping ADR-0002's stages (see [CI](#ci)); literal `.gitlab-ci.yml` intentionally absent per ADR-0002 | the badge at the top; [ADR-0002](docs/adr/0002-ci-github-actions.md) |
| Playwright | `e2e/specs/happy-path.spec.ts` — login, tenant switch + isolation, PR wizard + submit, self-approval block, convert → issue → acknowledge → GRN, invoice + attachment + 3-way match; `e2e/specs/worker-crash-replay.spec.ts` (#60) — SIGKILL the worker mid-flow (gated on observed in-flight queue state), durable RabbitMQ backlog with consumers == 0, story finished while the worker is dead, replay after restart with every event exactly once per entity, no duplicate projections/notifications/receipts, PO `received` in API + UI | `make e2e`; CI **E2E** job; #59/#60 dev notes |
| AI-assisted development | `AGENTS.md` (per-issue workflow, gates), `docs/ai-workflow.md`, a per-issue dev note for every merged issue in `docs/dev-notes/` | this repo's history: one issue per branch/PR, conventional commits |

### PRD §10 — MVP in-scope

| Capability | Where it lives (verified) | Evidence: tests / CI / docs |
|---|---|---|
| Auth (email + password), JWT, refresh, RBAC | `services/identity/auth` (argon2id, access + refresh rotation, revocation), `proto/identity/v1` (Login/Refresh/Logout/GetJwks), `services/gateway/server/auth.go`; roles admin/requester/approver/buyer/finance/viewer in `internal/platform/rbac` | identity coverage gate 80% in CI; `docs/rbac.md`; JWKS rotation runbook `docs/ops/jwks-rotation.md` |
| Multi-tenant (seed + demo switch) | tenant RLS `services/identity/migrations/0002_tenant_rls.sql`; tenant context `internal/platform/tenant`; every query scoped by `tenant_id`; cross-tenant reads return 404 | cross-tenant denial cases in gateway/identity tests; tenant switcher asserted in `make e2e` (#50) |
| Vendor master (CRUD, status, attachments) | `services/procurement/vendor` + `services/gateway/server/vendors.go` (create/update/deactivate, unique code per tenant); attachments via presigned MinIO uploads (`attachments.go`, `services/documents/attachment`) | vendor + attachment store/route tests; e2e creates vendors |
| Purchase Requisition (items, attachments, submit, cancel) | `services/procurement/requisition` (line items, draft→submit state machine, per-tenant numbering, idempotency); attachments on requisitions; wizard at `apps/web/src/app/(app)/app/requisitions/new`. Cancel exists as a domain transition (`requisition.go`, `requisition_cancelled` event) with **no REST/UI surface yet** | requisition domain/store/list tests; #28–#34 dev notes; e2e wizard step |
| Approval workflow (sequential steps, thresholds, reason) | `services/procurement/approval` (role + threshold rules via `PUT /api/v1/approval-rules`, decide with reason, self-approval forbidden even for admins, full history via outbox) | #30/#31 dev notes; e2e asserts both self-approval halves; admin settings UI |
| Purchase Order (from approved PR; issue, acknowledge, cancel) | `services/procurement/purchaseorder` — one-click convert (cannot convert twice), issue/acknowledge/cancel/close with 409 on invalid transitions | #35/#36 dev notes; p2p suite; e2e PO lifecycle. Convert UI is tracked in #112 (spec calls the gateway directly) |
| Goods Receipt (partial, over-receipt rejection) | `services/procurement/goodsreceipt` — partial receipts, cumulative qty ≤ ordered, over-receipt rejected with typed error, `grn.posted` event | #37/#38 dev notes; p2p + e2e full-GRN step |
| Vendor Invoice + 3-way match (tolerance) | `services/procurement/{invoice,match}` — manual entry + attachment, match PO vs GRN vs invoice with configurable qty/price tolerance (`/api/v1/match-settings` + admin UI), matched/discrepancy with explanation lines | #39–#41 dev notes; e2e asserts verdict "Matched"; invoice tests |
| Dashboard (pending approvals, spend, PO status) | `services/procurement/dashboard` + `/api/v1/dashboard/summary`; `/app/dashboard` KPI cards, spend by vendor, PO status distribution | #52 dev note; p95 budget `make perf-budget` on 100k fixtures (#56) |
| Scheduled nightly export (CSV→MinIO) + manual export | worker cron under Redis lock with heartbeat + missed-run metric (#49); manual trigger `/api/v1/exports` + reports page with run history and download URLs (#53) | #49/#53 dev notes; `make test-p2p` event flow; reports UI |
| Notifications (in-app) | **Implemented:** `notifications` projection in MongoDB (`services/documents/notifications`) fed by the worker consumer (#47). **Pending:** in-app notifications UI surfacing; email/SMTP stub not built | #47 dev note; p2p consumer assertions |
| Audit/event history viewer per document | `/api/v1/events` + document timelines on every detail page (#34/#43); immutable audit log with admin view + CSV export (#44) at `/app/audit` | #42–#44 dev notes; e2e timeline; `docs/schema/documents.md` |

## Demo & review artifacts

- **Demo script (seeded story)** — issue #63 (open; will walk PRD §22's 5-minute story)
- **5-minute demo video (EN subtitles)** — issue #64 (open)
- **Live-demo tunnel runbook (Cloudflare Tunnel)** — [docs/ops/live-demo-tunnel.md](docs/ops/live-demo-tunnel.md) (#65)
- **Hosting context** — [ADR-0003](docs/adr/0003-demo-hosting.md): local-first demo; no always-on public deployment in Phase 0

The reproducible stand-ins for the demo story are `make e2e` (the same sequence, asserted, plus the worker-crash resilience of step 6) and the resilience flow in `make test-p2p`.

## Development workflow (Git Flow)

- `main` — release/demo branch (protected)
- `develop` — integration branch (protected)
- `feature/<issue>-<slug>` → PR to `develop`
- `release/vX.Y.Z` → merge to `main` + tag → back-merge to `develop`
- `hotfix/<slug>` → `main` + `develop`

See [CONTRIBUTING.md](CONTRIBUTING.md). Issues use `type:*`, `area:*`, `priority:*`, `size:*` labels and milestones **M0–M5**.

## CI

`.github/workflows/ci.yml` runs on pushes and pull requests to `develop` and `main` (plus manual dispatch) — four jobs mapping ADR-0002's stages:

- **Go** — `go vet`, `go build`, duration-gated unit suite, migrations, full `go test` with Postgres/Mongo/MinIO service containers, coverage gates (identity ≥80%, procurement parity set ≥80%, documents report-only)
- **Web** — `pnpm install --frozen-lockfile`, `pnpm build`, `pnpm lint`, `pnpm test`
- **Integration** — procure-to-pay suite (`make test-p2p`) against compose Postgres/Mongo/RabbitMQ
- **E2E** — Playwright suite against the full compose stack: happy path + worker-crash replay resilience (builds all five service images)

> GitHub Actions minutes for private repositories depend on the account plan (Free includes 2,000 Linux minutes/month). Public repositories use free standard runners.

## Docs

- Architecture decisions — [ADR-0001 architecture](docs/adr/0001-architecture.md) · [ADR-0002 CI](docs/adr/0002-ci-github-actions.md) · [ADR-0003 demo hosting](docs/adr/0003-demo-hosting.md) · [ADR index](docs/adr/README.md)
- Local onboarding — [`docs/onboarding.md`](docs/onboarding.md)
- Schemas — [`docs/schema/identity.md`](docs/schema/identity.md) · [`docs/schema/procurement.md`](docs/schema/procurement.md) · [`docs/schema/documents.md`](docs/schema/documents.md)
- RBAC model — [`docs/rbac.md`](docs/rbac.md)
- Performance budgets — [`docs/perf.md`](docs/perf.md) (`make perf-budget`)
- Ops runbooks — [`docs/ops/`](docs/ops/) (JWKS rotation, DLQ monitor)
- PRD v0.2 — [`docs/prd/prd-v0.2.md`](docs/prd/prd-v0.2.md)
- Backlog map — [`docs/backlog-v0.1.md`](docs/backlog-v0.1.md)
- Agent workflow — [`AGENTS.md`](AGENTS.md) · AI-assisted workflow — [`docs/ai-workflow.md`](docs/ai-workflow.md)
- Per-issue dev notes — [`docs/dev-notes/`](docs/dev-notes/)
