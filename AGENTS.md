# AGENTS.md — Engineering workflow for AI coding agents

This repository is built issue-by-issue with Git Flow. ZCode (and other coding agents) read this file at the start of every task. Follow it exactly. When something is ambiguous, ask in the issue instead of guessing.

## Project snapshot

- eProcurement reference implementation: **Go microservices + Next.js**.
- Services: `gateway`, `identity`, `procurement`, `documents`, `worker`; web app in `apps/web`.
- Data: PostgreSQL + MongoDB + Redis + RabbitMQ + MinIO; OpenTelemetry collector.
- Read first: `README.md`, `docs/prd/prd-v0.2.md`, `docs/adr/`, `docs/backlog-v0.1.md`, `CONTRIBUTING.md`.
- Board: GitHub Project **eProcurement v0.1**; milestones **M0–M5**.

## Golden rules (non-negotiable)

1. **Never commit directly to `main` or `develop`.** Branch → PR → CI → merge.
2. **One issue per branch/PR.** Do not fix unrelated things in the same PR.
3. **Read before coding:** the issue, the PRD section it references, and any linked ADR.
4. **Keep diffs small and reviewable.** Prefer vertical slices over broad refactors.
5. **No secrets** in code, logs, prompts, commit messages, or screenshots. Use `.env.example` only.
6. **Never force-push shared branches** and never rewrite history.
7. **Run the full local gate set** before pushing (see below).
8. **Write a dev note** at `docs/dev-notes/<issue>-<slug>.md`: what changed, decisions, evidence.
9. **Language:** talk to the maintainer in Bahasa Indonesia; code, comments, commits, and docs in English.
10. **Ask before** changing architecture, adding dependencies, altering CI, or touching infrastructure.

## Per-issue workflow (Git Flow)

1. **Sync**
   ```bash
   git fetch origin
   git checkout develop
   git pull --ff-only
   ```
2. **Branch**
   ```bash
   git checkout -b feature/<issue>-<slug>   # bugs: fix/<issue>-<slug>
   ```
3. **Plan** — restate the acceptance criteria, list dependencies, and note the files you expect to touch.
4. **Implement** — smallest coherent change; add tests; update the dev note and any docs affected.
5. **Gates (all must pass)**
   ```bash
   go vet ./... && go build ./... && go test ./...
   pnpm install --frozen-lockfile && pnpm build && pnpm lint && pnpm test
   ```
   - Compose changes: `docker compose config`, `docker compose up -d --wait`, `docker compose ps`.
   - Seed changes: `make seed` (must stay under 2 minutes) and `make seed-reset` (demo data must be empty afterwards).
6. **Commit** — Conventional Commits referencing the issue:
   ```text
   feat(procurement): create PO from approved requisition (#28)
   ```
7. **Push and open a PR**
   ```bash
   git push -u origin HEAD
   gh pr create --base develop --fill
   ```
   PR body must contain: Summary, Verification (commands + results), `Closes #<issue>`, and risks/follow-ups.
8. **CI** — wait for both jobs (`Go`, `Web`) to pass. Fix failures; never bypass or disable checks.
9. **Merge** — squash-merge only when CI is green. Do not delete branches unless the maintainer asks.
10. **Report** — 3–6 lines: what changed, evidence, follow-ups, and the PR link.

## Commands cheat sheet

```bash
make help                 # list targets
make install              # pnpm install
make build                # go build ./... + pnpm build
make lint                 # go vet + pnpm lint
make test                 # go test + pnpm test
make fmt                  # gofmt services internal scripts

make compose-up           # start stack, wait for healthy services
make compose-down         # stop stack (keeps volumes)
make compose-logs         # follow logs
make compose-ps           # show health

make seed                 # reset + generate deterministic demo data
make seed-reset           # wipe demo data only
make reset                # drop compose volumes and recreate the stack

go run ./scripts/seed --tenants 2 --requisitions 10000 --events 100000   # lighter seed
```

Local endpoints: PostgreSQL `localhost:55432` (local override), MongoDB `27017`, Redis `6379`, RabbitMQ `5672` / UI `15672`, MinIO `9000` / `9001`, OTel `4317` / `4318` / `13133`.

## Implementation conventions

- **Go:** standard library first; `pgx` for PostgreSQL; official `mongo-driver`; table-driven tests; shared helpers in `internal/platform`; hexagonal layout (domain → application → adapters).
- **Web:** Next.js App Router + strict TypeScript; TanStack Query/Table; Zustand; React Hook Form + Zod; Tailwind.
- **Errors:** wrap with context; never log secrets; return typed errors at API boundaries.
- **Multi-tenancy:** every query is scoped by `tenant_id`; tests must include a cross-tenant denial case.
- **Async:** transactional outbox, idempotent consumers (`event_id`), retries with backoff, visible DLQ.
- **Observability:** spans and metrics for new endpoints/jobs; include `trace_id` in structured logs.

## Definition of done

- Acceptance criteria met and demonstrated.
- Tests cover the happy path and at least one failure path.
- All local gates pass; CI is green.
- Dev note and docs updated; no secrets; no unrelated changes.
- PR is linked to the issue, small, and reviewable.

## Kickoff prompt (paste into ZCode)

```text
Kerjakan issue #<N> mengikuti AGENTS.md di repo ini.
- Baca issue #<N> + PRD/ADR terkait.
- Branch dari develop: feature/<N>-<slug>.
- Implementasi sesuai acceptance criteria, diff kecil, tambah test.
- Jalankan gates lengkap (go + pnpm; compose/seed jika relevan).
- Commit conventional + push + buka PR ke develop dengan "Closes #<N>" dan bukti verifikasi.
- Tunggu CI hijau, lalu laporkan ringkas. Jangan menyentuh issue lain.
```
