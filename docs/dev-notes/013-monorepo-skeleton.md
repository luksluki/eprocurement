# 013 — Monorepo skeleton

- **Issue:** #13
- **Branch:** feature/13-monorepo-skeleton
- **Date:** 2026-10-20

## What
Single workspace with pnpm + Turborepo for JS packages and one Go module with multiple service binaries.

## Why
Issue #13 requires a reproducible root workspace before any service exists. One Go module keeps iteration fast while the service boundaries stay explicit (ADR-0001).

## Layout
- apps/web — placeholder package until the Next.js shell (#50)
- packages/ui, packages/config — placeholder shared packages
- services/*/cmd/* — five Go binaries sharing internal/platform/config
- Makefile — install/build/lint/test/fmt targets; compose/seed arrive later

## Evidence
- pnpm install, pnpm build, go build ./..., make help (commands and output in the PR).