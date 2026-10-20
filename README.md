# eProcurement — ELCODE ProcureKit

Multi-tenant mini eProcurement platform (procure-to-pay slice) built as an engineering showcase: **Go microservices + Next.js**, PostgreSQL + MongoDB + Redis + RabbitMQ, OpenTelemetry, Docker/Kubernetes manifests.

**Status:** planning — backlog lives in GitHub Issues + Project board. PRD: [`docs/prd/prd-v0.2.md`](docs/prd/prd-v0.2.md).

## Architecture (target)

```text
web (Next.js) → gateway (GoFiber, REST/BFF) → identity / procurement / documents (gRPC)
procurement → transactional outbox → RabbitMQ → worker (consumers, cron, exports)
PostgreSQL · MongoDB · Redis · RabbitMQ · MinIO · OTel/Jaeger · Prometheus
```

## Repository layout (target)

```text
apps/web/                    Next.js App Router + TypeScript
services/gateway/            Go · REST edge/BFF
services/identity/           Go · users, roles, login, JWKS
services/procurement/        Go · PR/approval/PO/GRN/invoice (PG + outbox)
services/documents/          Go · event history, audit, metadata (MongoDB)
services/worker/             Go · RabbitMQ consumers, cron, DLQ, exports
packages/{ui,config}/        shared React components + tooling presets
proto/ · gen/go/             gRPC contracts
deploy/{compose,k8s}/        local stack + kind manifests
docs/                        PRD, ADRs, AI workflow, performance notes
e2e/                         Playwright
scripts/                     seed, reset, migrate, gen-proto
```

## Development workflow (Git Flow)

- `main` — release/demo branch (protected)
- `develop` — integration branch (protected)
- `feature/<issue>-<slug>` → PR to `develop`
- `release/vX.Y.Z` → merge to `main` + tag → back-merge to `develop`
- `hotfix/<slug>` → `main` + `develop`

See [CONTRIBUTING.md](CONTRIBUTING.md). Issues use `type:*`, `area:*`, `priority:*`, `size:*` labels and milestones **M0–M5**.

## Quickstart

_Skeleton only (issue #13). Service behaviour arrives with the individual issues._

```bash
pnpm install      # install workspace tooling (Turborepo)
go build ./...    # compile the five service skeletons
make help         # list developer targets
```

Docker Compose arrives in #14; the full quickstart will be completed there.

## Docs

- PRD v0.2 — `docs/prd/prd-v0.2.md`
- Architecture decisions — `docs/adr/`
- AI-assisted workflow — `docs/ai-workflow.md`
- Backlog map — `docs/backlog-v0.1.md`
