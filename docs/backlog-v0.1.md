# Backlog map — v0.1 (2026-10-20)

Source of truth: **GitHub Issues + Project board**.

- Repository: https://github.com/luksluki/eprocurement
- Project: **eProcurement v0.1** — https://github.com/users/luksluki/projects/4
- Backlog: **12 epics (#1–#12) + 54 issues (#13–#66)**, 6 milestones, 20 labels.

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
| #1 | Platform & Delivery Foundation | M0 |
| #2 | Identity & Access | M1 |
| #3 | Vendor Management | M1 |
| #4 | Purchase Requisitions & Approvals | M2 |
| #5 | Purchase Orders & Goods Receipt | M3 |
| #6 | Invoice Matching | M3 |
| #7 | Documents, History & Audit | M2–M3 |
| #8 | Async Platform & Jobs | M2–M3 |
| #9 | Web Experience & Dashboard | M1–M4 |
| #10 | Observability & Performance | M4 |
| #11 | Testing & Quality | M4–M5 |
| #12 | Demo, Documentation & Release | M5 |

## Label taxonomy

- `type:*` — epic, feature, bug, chore, adr
- `area:*` — platform, identity, procurement, documents, async, web, observability, testing, demo
- `priority:*` — P0, P1, P2
- `size:*` — S, M, L

## Execution rules

- Branch from `develop` using `feature/<issue>-<slug>`; PR back to `develop`; squash merge.
- Every PR links its issue and meets the Definition of Done in `CONTRIBUTING.md`.
- Issues are sized ≤2 days; split `L` issues before starting.
- Project Status columns: Todo / In Progress / Done (built-in field). Labels remain the source of truth for epic/area/priority/size.
