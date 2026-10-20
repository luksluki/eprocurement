# eProcurement — ELCODE ProcureKit

[![CI](https://github.com/luksluki/eprocurement/actions/workflows/ci.yml/badge.svg)](https://github.com/luksluki/eprocurement/actions/workflows/ci.yml)

Multi-tenant eProcurement platform (procure-to-pay slice) and reference implementation: **Go microservices + Next.js**, PostgreSQL + MongoDB + Redis + RabbitMQ, OpenTelemetry, Docker/Kubernetes manifests.

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

## CI

`.github/workflows/ci.yml` runs on pushes and pull requests to `develop` and `main` (plus manual dispatch):

- **Go** — `go vet`, `go build`, `go test`
- **Web** — `pnpm install --frozen-lockfile`, `pnpm build`, `pnpm lint`, `pnpm test`

> GitHub Actions minutes for private repositories depend on the account plan (Free includes 2,000 Linux minutes/month). Public repositories use free standard runners.

## Quickstart

Prerequisites: Go 1.24+, Node 22+, pnpm, and Docker Desktop (or Docker Engine with Compose v2).

```bash
cp .env.example .env   # local-only credentials
make install           # pnpm install
make compose-up        # Postgres, MongoDB, Redis, RabbitMQ, MinIO, OTel collector
make build             # go build ./... + turbo build
make help              # all targets
```

| Service | Endpoint |
|---|---|
| PostgreSQL | `localhost:5432` |
| MongoDB | `localhost:27017` |
| Redis | `localhost:6379` |
| RabbitMQ (AMQP / UI) | `localhost:5672` / http://localhost:15672 |
| MinIO (S3 / Console) | http://localhost:9000 / http://localhost:9001 |
| OTel collector (gRPC / HTTP / health) | `localhost:4317` / `localhost:4318` / http://localhost:13133 |

Reset the local stack (drops volumes): `make reset`.

> Port conflict? If a service port is already used on your machine (for example a local PostgreSQL on 5432), override it in `.env` — e.g. `POSTGRES_PORT=55432` — and update the connection string accordingly.

Full walkthrough with expected output, troubleshooting, and demo-data reset guide: [`docs/onboarding.md`](docs/onboarding.md).

## Demo data

`make seed` wipes and regenerates a deterministic dataset: 3 tenants, users, vendors, **100,000 requisitions**, and **1,000,000 domain events** (PostgreSQL `demo` schema + MongoDB `events`). It completes in under two minutes on a laptop.

```bash
make seed          # reset + generate (deterministic, seed=42)
make seed-reset    # wipe demo data only
make reset         # drop compose volumes and recreate the stack
```

Override sizes for a lighter run:

```bash
go run ./scripts/seed --tenants 2 --requisitions 10000 --events 100000
```

The tool reads `DATABASE_URL`, `MONGO_URI`, and `MONGO_DB` from `.env` (or the environment).

## Docs

- Local onboarding — `docs/onboarding.md`
- PRD v0.2 — `docs/prd/prd-v0.2.md`
- Architecture decisions — `docs/adr/`
- Agent workflow — `AGENTS.md`
- AI-assisted workflow — `docs/ai-workflow.md`
- Backlog map — `docs/backlog-v0.1.md`
