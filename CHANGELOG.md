# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-11-02

First tagged release: the multi-tenant procure-to-pay slice — requisition →
approval → purchase order → goods receipt → 3-way invoice match, plus event
history, exports, and observability — built across milestones M0–M4 and
gated end-to-end by the four CI jobs (Go, Web, Integration, E2E). The full
capability-to-evidence map lives in the
[README capability matrix](README.md#capability-to-evidence-matrix); every
merged issue has a dev note under `docs/dev-notes/` (013–063).

### Added

- **Platform & delivery (M0)** — pnpm + Turborepo monorepo; five Go services
  (`gateway`, `identity`, `procurement`, `documents`, `worker`) in a hexagonal
  layout plus the Next.js app in `apps/web`; Docker Compose stack (11 default
  services: PostgreSQL, MongoDB, Redis, RabbitMQ, MinIO, OTel collector);
  four-job CI (Go / Web / Integration / E2E); ADRs 0001–0003; deterministic
  seed (`make seed`: 3 tenants, 100,000 requisitions, 1,000,000 domain
  events, budget-gated under two minutes).
- **Identity, RBAC & multi-tenancy (M1)** — email + password auth
  (argon2id), JWT access tokens with rotating, revocable refresh tokens, and
  a JWKS endpoint with rotation runbook; roles
  admin/requester/approver/buyer/finance/viewer; tenant isolation via
  PostgreSQL row-level security and `tenant_id`-scoped queries — cross-tenant
  reads return 404 (asserted in unit tests and the E2E suite).
- **Vendor master (M1)** — per-tenant vendors with unique codes, lifecycle
  status, and attachments via presigned MinIO uploads.
- **Requisitions & approvals (M2)** — requisitions with line items,
  attachments, draft→submit state machine, per-tenant numbering, and
  idempotent submission; sequential approval workflow driven by role and
  spend-threshold rules, decisions recorded with reason, self-approval
  forbidden for everyone (admins included), full decision history emitted
  through the transactional outbox.
- **Purchase orders & goods receipts (M3)** — one-click convert from an
  approved requisition (double-convert rejected), issue / acknowledge /
  cancel / close lifecycle with 409 on invalid transitions; partial goods
  receipts with cumulative-quantity enforcement and a typed over-receipt
  rejection (`grn_over_receipt`).
- **Vendor invoices & 3-way match (M3)** — invoice entry with attachments;
  match of PO vs GRN vs invoice under configurable quantity/price tolerances
  (admin settings API + UI), matched/discrepancy verdicts with per-line
  explanations.
- **Async platform & jobs (M2–M3)** — transactional outbox; RabbitMQ topology
  with dead-letter exchange and TTL retry queues; idempotent consumers
  (`event_id` dedupe); visible DLQ with monitor and admin replay; nightly
  CSV export to MinIO under a Redis lock with heartbeat and missed-run
  alerting; manual export trigger with run history and downloads.
- **Documents, history & audit (M2–M3)** — MongoDB event projections with
  document timelines on every detail page; in-app notifications projection;
  immutable audit log with admin view and CSV export.
- **Web experience (M1–M4)** — login, dashboard (pending approvals, spend,
  PO status), requisition wizard, approvals, purchase orders, vendors,
  invoices with match, reports/exports, audit, admin; i18n (EN/ID); TanStack
  Query/Table, React Hook Form + Zod, Tailwind.
- **Observability & performance (M4)** — OpenTelemetry spans across
  REST → gRPC → pgx → outbox publish → RabbitMQ consume → Redis; Prometheus
  metrics (ports 9100–9104) with Grafana dashboard and alert rules; list/read
  indexes, EXPLAIN notes, and a performance budget against 100k-requisition
  fixtures; Playwright happy-path E2E covering tenant isolation, the
  self-approval block, and the full procure-to-pay story.
- **Demo & docs (M5)** — presenter runbook
  [`docs/demo-script.md`](docs/demo-script.md) with a repeatable seeded story
  (`make demo` / `make demo-reset`), plus onboarding, schema, and RBAC docs.

### Known gaps / deferred

Honest state at release time (decision record: issue #66 comments).

- **#60** (worker-crash resilience test) and **#61** (load test) are
  consciously deferred by maintainer decision — v0.1.0 ships without them.
- **#64** — the 5-minute demo video: the final YouTube-unlisted URL is
  pending the recording session. The MP4 and `demo-captions.en.srt` ship as
  assets on this release's GitHub Release page (the fallback-hosting pattern
  documented in PR #124), and the URL is swapped in with a small commit once
  recorded.
- **#65** — the live-demo tunnel runbook does not exist yet.
- **#110** (seed data loaded into the live schema), **#112** (convert-to-PO
  UI), **#115** (DLQ alerting rules), and **#118** (store interface refactor)
  remain open; all four were assessed skip-safe for this release.
- Smaller product gaps stay labeled per capability in the
  [README capability matrix](README.md#capability-to-evidence-matrix) — e.g.
  the Redis read-model cache layer, the requisition cancel REST/UI surface,
  the notifications UI, the email stub, and the Kubernetes manifests (D4).

[0.1.0]: https://github.com/luksluki/eprocurement/releases/tag/v0.1.0
