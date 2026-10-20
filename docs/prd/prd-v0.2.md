# ELCODE ProcureKit
## Product Brief & Product Requirements Document (PRD)

**Version:** 0.2 (decisions resolved — 2026-10-20)
**Status:** Working Product Definition
**Brand:** ELCODE — Independent Software Studio
**Product:** ELCODE ProcureKit (working name)
**Purpose:** Showcase product — enterprise full-stack engineering evidence (Skyhi Senior/Mid-Senior Full-Stack JD, Role C: eProcurement)
**Initial market:** Indonesia (demonstration only; not a commercial launch in Phase 0)
**Author:** Shion (for R. Luki Rahayu Agung) — 2026-10-20

> **Product North Star:** A complete procure-to-pay slice that **runs, recovers, and explains itself** — every state change is traceable, every async failure is visible, every query is fast enough to feel invisible.

---

## 1. Executive Summary

ProcureKit is a **mini, multi-tenant eProcurement platform** built as a portfolio showcase for enterprise full-stack engineering. It implements one coherent business slice end-to-end:

```text
VENDOR
  → PURCHASE REQUISITION (PR)
  → APPROVAL WORKFLOW
  → PURCHASE ORDER (PO)
  → GOODS RECEIPT (GRN, partial allowed)
  → VENDOR INVOICE (3-way match)
  → SPEND DASHBOARD + SCHEDULED EXPORTS
```

It is deliberately **not** a commercial ERP replacement. Its value is technical evidence: Go microservices with clean/hexagonal architecture, gRPC, PostgreSQL + MongoDB + Redis, RabbitMQ with outbox/DLQ/retries, cron with distributed locks, a data-heavy Next.js UI, OpenTelemetry, Docker/Kubernetes, GitLab CI, Playwright — mapped one-to-one to the target job description.

## 2. Vision

**Vision statement.** Demonstrate, in a running system, that ELCODE can own enterprise procurement features from requirement to production — including the unglamorous parts: state machines, idempotency, retries, auditability, and multi-company isolation.

**Product promise.** *A 5-minute demo that survives a 60-minute technical interview.*

**Core product thesis.** Hiring evidence for enterprise roles is not a list of frameworks; it is a **recoverable system**: kill the worker and nothing is lost, replay an event and nothing is duplicated, open a trace and the whole request is visible.

## 3. Strategic Positioning

**Category.** Reference implementation / engineering showcase (not a commercial SaaS in Phase 0).

**Positioning boundaries.**
- Not a full ERP; not an accounting suite; not a sourcing marketplace.
- No payment gateway integration in MVP (invoice matching only).
- No mobile app, no POS, no offline-first (those belong to other roles).

**What success looks like.** A reviewer can open the repo, run `docker compose up`, follow the demo script, and tick every "Must-Have" line of the JD from observable behaviour — not from claims.

## 4. Showcase Objectives & JD Alignment

| Objective | Evidence |
|---|---|
| Show Go microservices with clean architecture | `services/gateway`, `services/procurement`, `services/documents`, `services/worker` — hexagon layers (domain/app/adapters) |
| Show gRPC + REST | REST edge (GoFiber) + gRPC/Protobuf internal contracts |
| Show PostgreSQL depth | Relational core, migrations, indexes, EXPLAIN notes, transactional outbox |
| Show MongoDB depth | Event history, audit log, document metadata, aggregation pipelines |
| Show Redis | Cache + distributed lock for cron |
| Show RabbitMQ | Topic exchange, routing, DLQ, retry with backoff, idempotent consumers |
| Show scheduled jobs | Nightly export with lock, heartbeat, missed-run detection |
| Show React/Next enterprise UI | Next.js + TanStack Query/Table + Zustand + RHF/Zod + Tailwind |
| Show observability | OpenTelemetry traces across REST → gRPC → worker; Prometheus metrics |
| Show delivery discipline | GitLab CI, Playwright e2e, PR reviews, ADRs, dev notes |
| Show AI-assisted development | `docs/ai-workflow.md` — prompts, review checklist, guardrails |

**Non-goal of the showcase:** breadth. Depth on one slice beats ten half-features.

## 4.1 Decision Log (2026-10-20)

| # | Decision | Outcome |
|---|---|---|
| D1 | Backend language | **All-Go** backend (gateway, identity, procurement, documents, worker) |
| D2 | Identity boundary | **Separate `identity` service** from day one (authN issuance + JWKS; gateway verifies only) |
| D3 | MongoDB boundary | **Events / audit / document metadata only** — no dual-write of business state |
| D4 | Kubernetes | **kind manifests only**, validated in CI; managed cluster explicitly out of scope |
| D5 | Demo hosting | Existing ELCODE VPS is **not sized for always-on** (≈1.6 GB RAM available, 9.4 GB disk free at decision time). Default: local demo + video, optional on-demand trimmed profile or VPS upgrade |

## 5. Target Users (demo personas)

- **Requester** — creates a purchase requisition for a department.
- **Approver** — approves or rejects based on thresholds and role.
- **Buyer / Procurement officer** — converts approved PRs to POs, manages vendors, records goods receipts.
- **Finance reviewer** — matches vendor invoices against PO + GRN, flags discrepancies.
- **Tenant admin** — manages users, roles, approval rules, and company data.

## 6. Personas

**Rina — Requester (tenant A).** Needs: fast PR form, attachments, visibility of approval status. Pain: re-typing data, unclear rejection reasons.

**Budi — Approver.** Needs: a queue sorted by age/amount, one-click approve/reject with reason, audit trail.

**Sari — Buyer.** Needs: convert PR → PO without re-entry; partial receipts; vendor master.

**Dewi — Finance.** Needs: 3-way match with tolerance rules; discrepancy notes.

## 7. Jobs To Be Done

- When a department needs to buy something, help me submit a complete requisition with the right budget context and attachments.
- When a request needs approval, show me only what needs my decision and why.
- When an approved request becomes an order, let me issue a PO without re-entering data.
- When goods arrive partially, record what actually arrived and keep the PO status truthful.
- When an invoice arrives, tell me whether it matches PO + GRN within tolerance — and if not, exactly where it diverges.
- When the month closes, give me a spend report and an export I can trust.

## 8. Core Product Loop

```text
CREATE PR → SUBMIT → APPROVE → ISSUE PO → RECEIVE (GRN) → MATCH INVOICE → REPORT/EXPORT
     ↑                                                                    |
     └──────────────────── audit trail + event history ───────────────────┘
```

Every step emits a domain event; every event is stored in an append-only history; the dashboard and exports are derived from these events plus relational state.

## 9. Product Principles

1. **Truth is relational; history is append-only.** Postgres owns current state; Mongo owns the immutable event/audit trail.
2. **Every async step is idempotent.** Replays must never duplicate receipts, invoices, or notifications.
3. **Tenant isolation is not optional.** `tenant_id` scoping in every query; RLS as defense-in-depth.
4. **The UI shows exceptions, not noise.** Approval queues and discrepancies first; vanity metrics later.
5. **Observable by default.** Every request carries `trace_id`; every job logs start/end/attempt.
6. **Showcase depth over breadth.** One slice, fully hardened.

## 10. Scope — MVP

**In scope**
- Auth (email + password), JWT, refresh, RBAC (admin, requester, approver, buyer, finance, viewer)
- Multi-tenant (create tenant via seed; switch tenant for demo)
- Vendor master (CRUD, status, attachments)
- Purchase Requisition (items, attachments, submit, cancel)
- Approval workflow (sequential steps, thresholds, approve/reject with reason, delegation out of scope)
- Purchase Order (from approved PR; issue, acknowledge, cancel)
- Goods Receipt (partial, over-receipt rejection, discrepancy note)
- Vendor Invoice (manual entry + attachment; 3-way match with tolerance; status: matched / discrepancy)
- Dashboard (pending approvals, spend by vendor/category, PO status distribution)
- Scheduled nightly export (CSV to MinIO) + manual export
- Notifications (in-app; email optional via SMTP stub)
- Audit/event history viewer per document

**Out of scope (MVP)**
- Sourcing events / RFQ / contracts
- Payment execution, bank integration, tax e-Faktur integration
- SAP integration (only an interface stub + ADR)
- Mobile, offline, POS
- Billing/subscriptions

## 11. Information Architecture

```text
/                      → redirect to dashboard
/login
/app/dashboard          → KPI cards, pending approvals, spend chart
/app/requisitions       → list + filters + pagination
/app/requisitions/new   → multi-step form (details → items → attachments → review)
/app/requisitions/:id   → detail + timeline + actions
/app/approvals          → queue (approver)
/app/purchase-orders    → list + detail + GRN actions
/app/vendors            → list + detail + documents
/app/invoices           → list + detail + match result
/app/reports            → exports, scheduled jobs, run history
/admin/users            → users, roles
/admin/settings         → approval rules, tolerances
```

## 12. Functional Requirements

| ID | Requirement | Acceptance criteria |
|---|---|---|
| FR-01 | Tenant-aware auth | Login returns JWT with `tenant_id` + roles; cross-tenant requests return 404/403; audit records actor |
| FR-02 | Vendor master | Create/edit/deactivate vendor; unique code per tenant; attachment upload via presigned URL |
| FR-03 | PR creation | Multi-line items with qty/price/currency; validation (qty > 0, currency allowed); draft → submit |
| FR-04 | Approval engine | Configurable steps (role + threshold); approve/reject with reason; PR status transitions only via domain service; full history |
| FR-05 | PO from PR | One-click convert; line items copied; PO number sequence per tenant; cannot convert twice |
| FR-06 | PO lifecycle | issue → acknowledged → partially_received → received → closed/cancelled; invalid transitions rejected with 409 |
| FR-07 | GRN | Partial receipts; over-receipt rejected; cumulative received qty cannot exceed ordered; emits `grn.posted` |
| FR-08 | Invoice match | 3-way match (PO, GRN, invoice) with configurable tolerance (qty ±%, price ±%); result stored with explanation lines |
| FR-09 | Dashboard | Pending approvals count, spend by vendor (30/90 days), PO status distribution; p95 < 300 ms on seed data |
| FR-10 | Scheduled export | Nightly CSV export to MinIO; Redis lock prevents overlap; missed-run detection (heartbeat age > 26 h → alert metric) |
| FR-11 | Event history | Every state change appears in document timeline within 2 s; events immutable; pagination + filters |
| FR-12 | Resilience demo | Killing the worker mid-job loses nothing; replaying an event does not duplicate records (idempotency keys) |

## 13. Data Model (summary)

**PostgreSQL (system of record — schema `procurement`)**
- `tenants(id, name, slug, created_at)`
- `users(id, tenant_id, email, password_hash, roles[], status)`
- `vendors(id, tenant_id, code, name, status, tax_id, created_at)`
- `requisitions(id, tenant_id, number, requester_id, status, currency, total, submitted_at)`
- `requisition_items(id, requisition_id, description, qty, unit_price, category)`
- `approval_steps(id, requisition_id, step_no, role_required, threshold, status, decided_by, decided_at, reason)`
- `purchase_orders(id, tenant_id, number, requisition_id, vendor_id, status, total, issued_at)`
- `po_items(id, po_id, description, qty_ordered, qty_received, unit_price)`
- `goods_receipts(id, tenant_id, po_id, number, received_by, received_at, notes)`
- `goods_receipt_items(id, grn_id, po_item_id, qty_received)`
- `invoices(id, tenant_id, number, po_id, vendor_id, amount, status, match_result JSONB, created_at)`
- `outbox(id, tenant_id, event_type, payload JSONB, created_at, published_at)`

**MongoDB (history & documents)**
- `events` — append-only domain events (`event_id` unique index, `tenant_id+occurred_at` index)
- `audit_log` — actor, action, entity, before/after, trace_id
- `document_meta` — attachments (owner, mime, size, object_key, checksum)
- `saved_views` — configurable table views (Phase 1.5)

**Redis**
- Cache: vendor list, category reference, dashboard aggregates (TTL 60 s, invalidated by events)
- Locks: `lock:export:nightly`, heartbeat `job:export:last_success`

**MinIO/S3**
- `attachments/{tenant_id}/{entity}/{uuid}` — presigned upload/download

## 14. Microservice Architecture

### 14.1 Context diagram

```text
                       ┌────────────────────────────┐
                       │  web (Next.js + TS)        │
                       │  TanStack Query/Table,     │
                       │  Zustand, RHF+Zod          │
                       └──────────────┬─────────────┘
                                      │ REST (JWT, tenant header)
                       ┌──────────────▼─────────────┐
                       │  gateway (Go · GoFiber)    │
                       │  JWT verify · BFF · limits │
                       └───┬──────────┬────────┬────┘
                           │ gRPC     │ gRPC   │ gRPC
                 ┌─────────▼───┐ ┌────▼────────┐ ┌▼─────────────────┐
                 │ identity    │ │ procurement │ │ documents        │
                 │ users/roles │ │ PR/PO/GRN/  │ │ events, audit,   │
                 │ JWKS        │ │ invoice     │ │ attachments meta │
                 │ Postgres    │ │ PG + outbox │ │ MongoDB          │
                 └─────────────┘ └──────┬──────┘ └────────▲─────────┘
                                        │ outbox → publish │ consume
                                        ▼                  │
                       ┌──────────────────────────────────────────┐
                       │ RabbitMQ: exchange `procurement.events`  │
                       │ queues: documents.history, notifications,│
                       │         exports; DLX: *.dlq              │
                       └───────────────┬──────────────────────────┘
                                       │
                             ┌─────────▼────────┐
                             │ worker (Go)      │
                             │ consumers + cron │
                             │ + Redis locks    │
                             └─────────┬────────┘
                                       │
                     Redis (cache/locks) · MinIO · SMTP stub
```

### 14.2 Service catalog

| Service | Language | Responsibility | Sync API | Data store | Scaling |
|---|---|---|---|---|---|
| `web` | TypeScript / Next.js | Enterprise UI, data fetching, client state | REST | — | stateless |
| `gateway` | Go / GoFiber | REST edge, JWT verification, RBAC, rate limiting, aggregation | REST → gRPC | — | stateless |
| `identity` | Go / gRPC | Users, roles, login, refresh tokens, JWKS | gRPC (internal) + login via gateway | PostgreSQL `identity` schema | stateless |
| `procurement` | Go / gRPC | Domain core: vendors, PR, approvals, PO, GRN, invoice match | gRPC (internal) | PostgreSQL + outbox | stateless |
| `documents` | Go / gRPC | Event history, audit log, document metadata | gRPC (internal) | MongoDB + MinIO | stateless |
| `worker` | Go | Consumers (history, notifications, exports), cron with Redis lock, DLQ retry | — | Redis + MinIO + Mongo | singleton per lock |

**Identity is its own service (D2, 2026-10-20).** `identity` owns users, roles, refresh tokens, and JWKS. The gateway verifies JWTs and enforces RBAC; it never stores credentials. This adds one small deployable but removes a future refactor and keeps auth as a clean bounded context.

### 14.3 Data ownership & consistency

- Each service owns its store; no shared tables across services.
- `procurement` is the **system of record**; `documents` is an **append-only projection** built from events (CQRS-lite).
- Consistency: **transactional outbox** in `procurement` (state + event in one transaction) → worker publishes → consumers dedupe by `event_id` (unique index in Mongo / Redis SETNX for side effects).
- Read-your-writes for the UI: gateway queries `procurement` synchronously; history may lag ≤ 2 s (documented).

### 14.4 Synchronous flows (gRPC)

- `gateway → procurement`: `CreateVendor`, `ListVendors`, `CreateRequisition`, `SubmitRequisition`, `DecideApproval`, `CreatePurchaseOrder`, `PostGoodsReceipt`, `CreateInvoice`, `MatchInvoice`, `GetDashboard`.
- `gateway → identity`: `Login`, `Refresh`, `ListUsers`, `AssignRoles`.
- `gateway → documents`: `ListEvents`, `GetDocumentMeta`, `CreatePresignedUpload`.
- Protobuf contracts in `proto/` with generated clients; versioning via package names (`procurement.v1`).

### 14.5 Asynchronous flows (RabbitMQ + outbox)

- Exchange: `procurement.events` (topic), routing keys `pr.*`, `po.*`, `grn.*`, `invoice.*`, `vendor.*`.
- Queues: `documents.history`, `notifications`, `exports`; each with `x-dead-letter-exchange: procurement.dlx` and retry queue with TTL backoff (5 s → 30 s → 5 min).
- Consumer rules: idempotency key = `event_id`; failures → retry; after max attempts → DLQ + metric + admin visible.
- Event envelope: `{event_id, tenant_id, type, version, occurred_at, actor_id, trace_id, payload}`.

### 14.6 Event catalog (MVP)

`vendor.created` · `pr.submitted` · `pr.approved` · `pr.rejected` · `po.issued` · `po.acknowledged` · `grn.posted` · `invoice.matched` · `invoice.discrepancy` · `export.requested` · `export.completed` · `export.failed`

### 14.7 Multi-tenancy & security

- `tenant_id` + roles in JWT; gateway injects tenant context into gRPC metadata.
- Service layer enforces tenant scoping; Postgres **RLS** on tenant-owned tables as defense-in-depth.
- Mongo: every document has `tenant_id`; compound indexes `{tenant_id, occurred_at}`.
- Attachments: presigned URLs scoped per tenant; checksum verified on completion.
- Audit: every decision (approve/reject/issue/post/match) writes `audit_log` with actor + trace.

### 14.8 Failure modes & recovery

| Failure | Behaviour | Recovery |
|---|---|---|
| RabbitMQ down | Outbox backlog grows; no data loss | Worker resumes publishing; idempotent consumers |
| Worker down | Events not delivered; cron skipped | Restart → lock TTL expires → missed-run detection alerts; replay from outbox |
| Mongo down | History unavailable (read degraded) | Queue retains messages; dashboard uses Postgres |
| Postgres down | Writes fail with 503; UI read-only | Restore; outbox already consistent |
| Duplicate delivery | Consumer dedupes by `event_id` | Safe by design |

### 14.9 Deployment topology

`docker compose` services: `web`, `gateway`, `identity`, `procurement`, `documents`, `worker`, `postgres`, `mongo`, `redis`, `rabbitmq`, `minio`, `otel-collector`, `jaeger`, `prometheus`. Optional `k8s/` manifests (kind): Deployments + Services + ConfigMaps + HPA for stateless services. A `demo` profile trims observability services and applies memory caps; see D5 for hosting constraints.

### 14.10 Repository layout (monorepo)

```text
procurekit/
├─ apps/
│  └─ web/                     # Next.js App Router + TS (pnpm workspace)
├─ services/
│  ├─ gateway/                 # Go · REST edge/BFF
│  ├─ identity/                # Go · authN/Z issuance, users, roles
│  ├─ procurement/             # Go · domain core (PG + outbox)
│  ├─ documents/               # Go · history/audit/metadata (Mongo)
│  └─ worker/                  # Go · consumers, cron, DLQ, exports
├─ packages/
│  ├─ ui/                      # shared React components (design system)
│  └─ config/                  # eslint / tsconfig / tailwind presets
├─ proto/                      # protobuf contracts (identity.v1, procurement.v1, documents.v1)
├─ gen/go/                     # generated Go stubs (buf/protoc)
├─ deploy/
│  ├─ compose/                 # docker-compose.yml + demo profile + env samples
│  └─ k8s/                     # kind manifests (validated in CI)
├─ docs/
│  ├─ adr/                     # ADR-0001.. ; ADR-0001 = architecture
│  ├─ perf.md · mongo.md · ai-workflow.md
├─ e2e/                        # Playwright
├─ scripts/                    # seed, reset, migrate, gen-proto
├─ go.work  (or single module with services/*/cmd)
├─ pnpm-workspace.yaml · turbo.json
├─ Makefile · .gitlab-ci.yml
└─ README.md
```

**Go module strategy.** One Go module with multiple binaries (`services/*/cmd/*/main.go`) and shared `internal/platform` (config, telemetry, db, auth middleware). Rationale: fast iteration for a showcase; the service boundaries stay explicit and can be split into modules later without changing APIs.

## 15. API Structure

**REST (gateway, OpenAPI 3.1)**
```
POST /api/v1/auth/login
GET  /api/v1/vendors?query=&page=&pageSize=
POST /api/v1/vendors
POST /api/v1/requisitions
POST /api/v1/requisitions/{id}/submit
POST /api/v1/approvals/{stepId}/decide
POST /api/v1/purchase-orders/from-requisition/{id}
POST /api/v1/purchase-orders/{id}/issue
POST /api/v1/purchase-orders/{id}/goods-receipts
POST /api/v1/invoices
POST /api/v1/invoices/{id}/match
GET  /api/v1/dashboard/summary
GET  /api/v1/events?entityType=&entityId=
POST /api/v1/exports
```

**gRPC (internal)**
```proto
service Procurement { rpc CreateRequisition(...) returns (...); rpc DecideApproval(...); ... }
service Documents   { rpc ListEvents(...) returns (...); rpc CreatePresignedUpload(...); ... }
```

## 16. Background Jobs & Scheduling

- **Nightly export** — 01:00 tenant-local; CSV per tenant to MinIO; Redis lock `lock:export:nightly:{tenant}`; heartbeat key updated on success; metric `export_age_seconds`; alert if > 26 h.
- **Outbox publisher** — polls `outbox` every 2 s (batch 50), publishes, marks `published_at`; retries on failure.
- **DLQ monitor** — hourly summary metric + admin endpoint.
- **SLA reminders** — daily check for POs awaiting acknowledgement > 3 days → notification event.

## 17. Security Requirements

- Password hashing (argon2id), JWT access (15 min) + refresh (7 days, rotation), logout revocation list in Redis.
- RBAC enforced at gateway + service layer (defense in depth).
- Input validation (Zod-equivalent server-side structs), output encoding, SQL via prepared statements, Mongo query whitelisting.
- Rate limiting per tenant/IP on auth and write endpoints.
- Secrets via env; no secrets in logs; attachments served via short-lived signed URLs.
- Audit trail immutable (append-only Mongo collection; no update/delete API).

## 18. Reliability & Observability

- **OpenTelemetry**: trace context propagated REST → gRPC → RabbitMQ headers → worker; spans around DB, cache, publish/consume.
- **Prometheus metrics**: request latency p50/p95/p99, error rate, outbox lag, DLQ depth, job last success age, cache hit ratio.
- **Structured logs**: JSON with `trace_id`, `tenant_id`, `actor_id`; no PII.
- **Dashboards**: Jaeger for traces; Grafana/Prometheus for metrics (compose optional).

## 19. Non-Functional Requirements

| Area | Requirement |
|---|---|
| Performance | p95 < 300 ms for list/detail endpoints on 100k-item seed; dashboard p95 < 500 ms |
| Scale (demo) | 10 tenants, 100k requisitions, 1M events — seed generator included |
| Availability | Compose demo: restart-safe; no data loss on worker kill |
| Security | OWASP Top 10 mitigations; tenant isolation verified by e2e test |
| Accessibility | Keyboard navigation on core flows; contrast AA on primary screens |
| i18n | UI copy ID + EN (cookie-based) |
| Portability | Runs on `docker compose up` with one command; no cloud dependency |

## 20. Technology Stack & JD Evidence Matrix

| JD requirement | Where it lives |
|---|---|
| Go microservices, clean/hexagonal | 5 Go services with `domain/app/adapters` layers |
| gRPC / Protobuf | `proto/` contracts; gateway↔services |
| REST | GoFiber OpenAPI at gateway |
| PostgreSQL (indexes, EXPLAIN, N+1) | migrations, composite indexes, `docs/perf.md` with EXPLAIN notes |
| MongoDB (schema, aggregation, pagination) | `documents` service; pipelines in `docs/mongo.md` |
| Redis (cache, TTL, lock) | cache layer + cron lock |
| RabbitMQ (exchange, DLQ, retry, outbox) | `worker` + `procurement/outbox` |
| Cron + failed-run monitoring | nightly export + heartbeat metric |
| React/Next + TanStack Query/Table + Zustand + RHF/Zod | `web/app/**` |
| Micro-frontend / monorepo (nice-to-have) | pnpm workspace + Turborepo: `apps/web`, `packages/ui`, `packages/config` |
| OpenTelemetry + Prometheus | `internal/telemetry` + compose stack |
| Docker + Kubernetes | compose + `k8s/` manifests |
| GitLab CI | `.gitlab-ci.yml`: lint → unit → integration → build → e2e |
| Playwright | `e2e/` happy path + resilience test |
| AI-assisted development | `docs/ai-workflow.md` (prompts, review checklist) |

## 21. Milestones (21 days, part-time)

| Day | Deliverable |
|---|---|
| 1–2 | Repo scaffold, compose stack, CI pipeline, health endpoints, ADR-0001 (architecture) |
| 3–5 | `identity` service + RBAC + tenant context; vendor CRUD; Next.js shell + auth |
| 6–8 | PR CRUD + approval engine + Postgres outbox; event history (Mongo consumer) |
| 9–12 | PO + GRN + invoice match; RabbitMQ retry/DLQ; nightly export + Redis lock |
| 13–16 | UI: PR form, approval queue, PO/GRN screens, dashboard (TanStack/Zustand/RHF/Zod) |
| 17–19 | OTel + metrics + perf tuning (indexes, EXPLAIN); Playwright e2e |
| 20–21 | README + architecture + demo video (EN subtitles) + deploy demo |

## 22. Demo Script (5 minutes)

1. Login as Requester (tenant A) → create vendor + PR with attachment → submit.
2. Switch to Approver → approve with note → show timeline (Mongo events).
3. As Buyer → convert to PO → issue → record **partial** GRN; attempt over-receipt (rejected).
4. As Finance → create invoice → run 3-way match → show tolerance config and discrepancy detail.
5. Open dashboard → spend by vendor; trigger export → show file in MinIO + job history.
6. **Resilience:** kill worker → trigger action → events queue; restart worker → everything drains, no duplicates (show idempotency log).
7. **Observability:** open Jaeger trace for the PO issue request; show spans across gateway → procurement → RabbitMQ → worker → documents.

## 23. MVP Acceptance Criteria

- [ ] `docker compose up` starts the full stack; seed data available.
- [ ] Full procure-to-pay happy path works from UI.
- [ ] Over-receipt and invalid state transitions are rejected with clear errors.
- [ ] Event history shows every state change within 2 s.
- [ ] Worker kill/restart loses no events and produces no duplicates.
- [ ] Nightly export runs under lock; missed-run metric visible.
- [ ] p95 latency meets NFR on seed dataset.
- [ ] Playwright e2e covers happy path + one resilience scenario.
- [ ] README maps every JD requirement to code/tests/demo steps.

## 24. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Scope creep (ERP gravity) | Freeze MVP scope; new ideas → Phase 2 backlog |
| Async complexity eats the timeline | Outbox + single worker first; add retry/DLQ in week 2 |
| Go ramp-up | Start with gateway; reuse patterns; AI-assisted coding with review checklist |
| Demo environment fragility | Compose-first; no cloud dependencies; seed + reset script |
| "Showcase smell" (too perfect) | Document real trade-offs and open decisions in ADRs |

## 25. Decisions (resolved 2026-10-20)

| # | Question | Decision |
|---|---|---|
| D1 | Backend language | All-Go backend |
| D2 | Identity boundary | Separate `identity` service from day one |
| D3 | MongoDB boundary | Events/audit/document metadata only |
| D4 | Kubernetes | kind manifests + CI validation; no managed cluster |
| D5 | Demo hosting | VPS is not sized for always-on; default local demo + video; optional on-demand trimmed profile or VPS upgrade |

**Hosting addendum (capacity check, 2026-10-20).** The shared ELCODE VPS reports 3.6 GB total RAM (≈1.6 GB available), 9.4 GB free disk, 2 vCPU, and 11 running containers (studio prod/staging, Postgres, MinIO ×2, GlitchTip ×3, portfolio). A trimmed ProcureKit demo stack (web + 5 Go services + Mongo + Redis + RabbitMQ; reusing Postgres/MinIO; no Jaeger/Prometheus) needs ≈1.0–1.4 GB and several GB of images — feasible only as an on-demand, memory-capped demo, not always-on. Managed Kubernetes is rejected on cost/ops grounds.

## 26. Summary

ProcureKit is a deliberately narrow, deliberately deep reference implementation: one procure-to-pay slice with enterprise-grade correctness, async resilience, observability, and a data-heavy UI. It converts "I have done enterprise SaaS" into "here is the system, run it and judge it" — which is exactly what the target JD asks a senior engineer to demonstrate.
