# 015 — GitHub Actions CI

- **Issue:** #15
- **Branch:** feature/15-github-actions-ci
- **Date:** 2026-10-20

## What
A two-job CI pipeline for the monorepo: Go checks and the pnpm/Turborepo workspace checks.

## Jobs
- `go` — checkout, setup-go (version from `go.mod`), `go vet ./...`, `go build ./...`, `go test ./...`
- `web` — checkout, pnpm (version from `packageManager`), Node 22 with pnpm cache, `pnpm install --frozen-lockfile`, `pnpm build`, `pnpm lint`, `pnpm test`

## Triggers
- push to `develop` and `main`
- pull requests targeting `develop` and `main`
- manual `workflow_dispatch`

Concurrency cancels superseded runs per ref; the workflow has read-only repository permissions.

## Notes
- Private repositories consume GitHub Actions minutes from the account plan (Free: 2,000 Linux minutes/month). Public repositories use free standard runners.
- The account currently has an Actions billing/spending-limit block (see `elcode-studio-os` runs failing in ~3 seconds), so this workflow cannot execute until billing is restored or the repository becomes public.
- Local equivalents were executed as evidence for this issue.

## Evidence (local, 2026-10-20)
- `go vet ./...` ✓, `go build ./...` ✓, `go test ./...` ✓
- `pnpm install --frozen-lockfile` ✓, `pnpm build` ✓, `pnpm lint` ✓, `pnpm test` ✓
