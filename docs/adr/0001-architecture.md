# ADR-0001 — Service architecture and data ownership

- **Status:** Accepted
- **Date:** 2026-10-20
- **Deciders:** Luks (owner), Shion (engineering)
- **Related:** PRD v0.2 §14

## Context

The platform must provide enterprise Go microservices with clean architecture, gRPC, PostgreSQL + MongoDB + Redis + RabbitMQ, transactional outbox, scheduled jobs, and a data-heavy Next.js frontend — while staying deliverable in ~21 part-time days.

## Decision

Adopt five Go services plus a Next.js web app:

1. `gateway` — REST edge/BFF (GoFiber): JWT verification, RBAC enforcement, rate limiting, request aggregation.
2. `identity` — users, roles, refresh tokens, JWKS (gRPC internal; login exposed via gateway).
3. `procurement` — domain core: vendors, requisitions, approvals, purchase orders, goods receipts, invoice matching. PostgreSQL + transactional outbox.
4. `documents` — append-only event history, audit log, document metadata. MongoDB + MinIO metadata.
5. `worker` — RabbitMQ consumers (history, notifications, exports), cron jobs with Redis locks, DLQ handling.

**Data ownership.** Each service owns its stores. `procurement` is the system of record; `documents` is an event-driven projection (CQRS-lite). No shared tables; no dual writes.

**Communication.** Synchronous: REST at the edge, gRPC internally. Asynchronous: transactional outbox → RabbitMQ topic exchange → idempotent consumers.

**Tenancy.** `tenant_id` in JWT claims, propagated via gRPC metadata; service-layer scoping plus PostgreSQL RLS as defense-in-depth; Mongo compound indexes on `{tenant_id, occurred_at}`.

**Deployment.** Docker Compose for local/demo; Kubernetes manifests for `kind` (no managed cluster). A `demo` profile trims observability services and applies memory caps.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| Modular monolith (single Go binary) | Fastest to build, but weak support for the required microservices architecture |
| BFF inside Next.js | Would remove the Go REST edge from the architecture |
| Identity inside gateway | Simpler for M0, but creates a refactor later and blurs the auth boundary |
| Kafka instead of RabbitMQ | RabbitMQ (exchanges, DLQ, retry) is an explicit architecture requirement |
| Shared database across services | Couples deployments and breaks ownership; poor enterprise evidence |

## Consequences

**Positive:** clear bounded contexts; strong architectural evidence; failure isolation; independent scaling.

**Negative:** more deployables (6 + infra); more local complexity; distributed tracing required from day one; more integration testing.

**Mitigations:** single Go module with multiple binaries; shared `internal/platform` package; compose `demo` profile; ADR-driven decisions; small issues (≤2 days).
