# 019 — Identity service skeleton with PostgreSQL schema

- **Issue:** #19
- **Branch:** feature/19-identity-skeleton
- **Date:** 2026-10-20

## What
- `identity` service: gRPC server serving `grpc.health.v1` (liveness `""` / readiness `"identity"` backed by a PostgreSQL ping loop) + reflection, with unary interceptors for trace-id propagation, JSON request logging (`method`, `code`, `duration_ms`, `trace_id`), and panic→`Internal` recovery. Graceful shutdown on SIGINT/SIGTERM. Applies migrations on boot.
- `identity` PostgreSQL schema via embedded migration `0001_init.sql`: `users`, `roles` (fixed PRD catalog, seeded idempotently), `user_roles`, `refresh_tokens` (sha256 hash, rotation via `rotated_to`, `revoked_at`), all tenant-scoped.
- `internal/platform/dbmigrate`: stdlib-first migrator (no new tool dependency) — per-file transactions, `identity.schema_migrations` tracking, idempotent, malformed `.sql` names fail loudly.
- `internal/platform/logger`: JSON slog + trace-id context helpers.
- Infra: generic `deploy/docker/service.Dockerfile` (any `services/<svc>/cmd/<svc>` + bundled `grpc-health-probe`), `identity` service in compose (depends on healthy postgres, gRPC healthcheck, migrations on boot), CI runs migrations against a Postgres service container + integration tests via `TEST_DATABASE_URL`.

## Decisions
- **grpc v1.80.0, not latest:** v1.84 requires Go ≥ 1.25; the repo pins Go 1.24 (README, Dockerfile, CI `go-version-file`). Pinned grpc v1.80.0 with its exact transitive pins (x/net v0.49.0, x/text v0.33.0, x/sys v0.40.0, x/sync v0.19.0, genproto 20260120) so `go mod tidy` keeps `go 1.24.0`.
- **Hand-rolled migrator** over golang-migrate/goose: AGENTS.md says stdlib-first and ask before dependencies; the need is ~100 lines (ordered files, tx + tracking table). Revisit if up/down or versioned tooling is ever required.
- **Multi-statement SQL** needs pgx simple-protocol mode: migrations run through a short-lived simple-protocol pool; the runtime pool keeps prepared statements.
- **No `identity.v1` proto yet** — Login/Refresh/ListUsers/AssignRoles arrive with the auth issues (epic #2); skeleton serves health + reflection only.
- Liveness (`""`) stays SERVING while the process runs; readiness (`"identity"`) tracks the DB ping — the split compose/K8s needs later.
- Cross-tenant denial tests and RLS (ADR-0001 defense-in-depth) belong to the query layer issue; schema already enforces tenant-scoped email uniqueness.

## Evidence
- `go vet ./... && go build ./...` clean; `go test ./...` green (unit: migrator ordering/rejection, logger trace-id, health liveness/readiness transitions, trace propagation, recovery, log shape; integration gated on `TEST_DATABASE_URL`).
- Integration run against local PG: migrator idempotent (second run applies 0), failed file leaves no record and a fixed rerun retries only it.
- `go run ./services/identity/cmd/migrate` twice → `applied 1` then `applied 0`; `identity` schema lists 5 tables, 6 roles seeded.
- Local run: service on :18081, probe (`scripts/grpc-health-probe`) SERVING for both `""` and `identity`; logs are JSON with generated `trace_id`.
- Compose: `docker compose up -d --wait --build` → all 7 services healthy (25.9 s incl. image build). **AC proof:** dropped `identity` schema, `docker compose restart identity` → migration re-applied (`schema_migrations`=1, roles=6) and readiness SERVING again; container logs show graceful shutdown, `migrations applied count=1`, health checks with trace ids.
- pnpm gates: install --frozen-lockfile / build / lint / test all pass.

## Follow-ups
- `identity.v1` proto + Login/Refresh/JWT/argon2id (next issues in epic #2).
- RLS + cross-tenant denial tests with the query layer.
- OTel spans/metrics for gRPC (M4, #54); `trace_id` in logs is the day-one slice.
