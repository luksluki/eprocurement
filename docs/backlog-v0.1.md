# Backlog map — v0.1 (2026-10-20)

Source of truth: **GitHub Issues + Project board**. This file is an orientation map, not a duplicate of every issue.

## Milestones

| Milestone | Window | Exit criteria |
|---|---|---|
| **M0 — Foundation** | Day 1–2 | Monorepo + compose stack + CI + ADRs + conventions; `make compose-up` works |
| **M1 — Identity, Vendors & Web Shell** | Day 3–5 | Login/RBAC/tenant context; vendor CRUD; Next.js shell |
| **M2 — Requisitions, Approvals & History** | Day 6–8 | PR flow with approval engine; event history consumer |
| **M3 — PO, GRN, Invoice & Jobs** | Day 9–12 | PO/GRN/invoice match; outbox + RabbitMQ + cron export |
| **M4 — UI, Observability & Tests** | Day 13–16 | Full UI flows; OTel/metrics; perf budget; Playwright |
| **M5 — Demo, Docs & Release** | Day 17–21 | README + demo video + tunnel runbook + v0.1.0 tag |

## Epics

| # | Epic | Milestones touched |
|---|---|---|
| E1 | Platform & Delivery Foundation | M0 |
| E2 | Identity & Access | M1 |
| E3 | Vendor Management | M1 |
| E4 | Purchase Requisitions & Approvals | M2 |
| E5 | Purchase Orders & Goods Receipt | M3 |
| E6 | Invoice Matching | M3 |
| E7 | Documents, History & Audit | M2–M3 |
| E8 | Async Platform & Jobs | M2–M3 |
| E9 | Web Experience & Dashboard | M1–M4 |
| E10 | Observability & Performance | M4 |
| E11 | Testing & Quality | M4–M5 |
| E12 | Demo, Documentation & Release | M5 |

## Label taxonomy

- `type:*` — epic, feature, bug, chore, adr
- `area:*` — platform, identity, procurement, documents, async, web, observability, testing, demo
- `priority:*` — P0, P1, P2
- `size:*` — S, M, L

## Execution rules

- Branch from `develop` using `feature/<issue>-<slug>`; PR back to `develop`; squash merge.
- Every PR links its issue and meets the Definition of Done in `CONTRIBUTING.md`.
- Issues are sized ≤2 days; split `L` issues before starting.

## Project board

GitHub Project: **eProcurement v0.1** (created 2026-10-20). Columns follow the built-in **Status** field (Todo / In Progress / Done); filters by `area:*` and milestone.
