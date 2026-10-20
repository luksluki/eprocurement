# 018 — Local onboarding documentation

- **Issue:** #18
- **Branch:** feature/18-onboarding-docs
- **Date:** 2026-10-20

## What
New `docs/onboarding.md` (prerequisites, fresh-clone walkthrough, env-variable reference, make-target guide, demo-data reset levels, troubleshooting) plus a README link and two optional vars (`APP_ENV`, `PORT`) added to `.env.example`.

## Decisions
- Details live in `docs/onboarding.md`; README keeps its quickstart and links out (per issue technical note).
- Env audit: every `${VAR}` in `compose.yaml` and every var read by `scripts/seed` and `internal/platform/config` is present in `.env.example`. `APP_ENV`/`PORT` (config helper, defaulted) were undocumented — added as optional.
- Documented the port-override pitfall: compose maps ports but does not rewrite connection strings, so `POSTGRES_PORT` and `DATABASE_URL` must be changed together (same for Mongo/RabbitMQ/MinIO/OTel pairs).
- Three reset levels tabulated: `seed-reset` (demo data only) < `seed` (reset + regenerate) < `reset` (drop volumes).

## Evidence
Fresh-clone walkthrough executed from a clean export of the branch (no `node_modules`, no `.env`), including the documented port-override path (host port 5432 occupied by a native PostgreSQL):
- `make install` → 0.7 s; `docker compose config` → OK.
- `make compose-up` (cold) → 12.5 s, all six services `healthy`; `make compose-ps` clean.
- `make build` → 6.0 s; `make lint && make test` → all tasks successful.
- `make seed` → 18.1 s (< 2 min budget), counts match dev-note 017 exactly (determinism confirmed); documented psql/mongosh sanity checks returned 100,000 / 1,000,000.
- `make seed-reset` → 1.1 s; `demo.requisitions` and `events` both empty afterwards.
- Main repo gates: `go vet/build/test` pass; `pnpm install --frozen-lockfile && pnpm build && pnpm lint && pnpm test` pass.

## Follow-ups
- Onboarding doc should grow a section for running application services locally once they get real entrypoints beyond the current skeletons.
