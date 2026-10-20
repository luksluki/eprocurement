# 014 — Docker Compose dev stack

- **Issue:** #14
- **Branch:** feature/14-compose-dev-stack
- **Date:** 2026-10-20

## What
Local stack for PostgreSQL, MongoDB, Redis, RabbitMQ, MinIO, and the OpenTelemetry collector, with health checks and named volumes.

## Decisions
- Compose project name `eprocurement`; single bridge network `backend`.
- All ports and credentials come from `.env` (`.env.example` committed); no secrets in the compose file.
- Every stateful service has a health check; `make compose-up` uses `docker compose up -d --wait`.
- OTel collector uses the contrib image with a minimal pipeline (debug exporter) and health extension on :13133.
- The observability profile (Jaeger/Prometheus/Grafana) is deferred to #54 to keep the demo footprint small.

## Evidence
- `docker compose config` validates.
- `docker compose up -d --wait` reports all services healthy; endpoints reachable from the host.
- `make reset` drops volumes and recreates a healthy stack.

## Notes
- The official `minio/minio` image is no longer available on Docker Hub (and `quay.io/minio/minio` is unreachable), so the local stack pins `bitnamilegacy/minio:2025.7.23-debian-12-r5`.
- The Bitnami image runs as a non-root user and cannot initialise the named volume on Docker Desktop; the compose file sets `user: "0:0"` for local development only.
- Local PostgreSQL already occupies port 5432, so `.env` overrides `POSTGRES_PORT=55432`; `.env.example` keeps the conventional 5432 default.
