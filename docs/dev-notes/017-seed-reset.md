# 017 — Seed and reset tooling

- **Issue:** #17
- **Branch:** feature/17-seed-reset
- **Date:** 2026-10-20

## What
A Go tool (`scripts/seed`) that creates the demo schema and generates a deterministic dataset for PostgreSQL and MongoDB, plus reset targets.

## Decisions
- Demo data lives in an isolated `demo` PostgreSQL schema and the `events` MongoDB collection; application schemas arrive with later issues.
- Fixed RNG seed (42) makes UUIDs, names, amounts, and event order deterministic.
- PostgreSQL uses `COPY` for bulk loads; MongoDB uses batched `InsertMany` (5,000 docs per batch).
- `make seed` always resets first so reruns are reproducible; `make seed-reset` wipes without seeding.
- Counts are configurable via flags (`--tenants`, `--requisitions`, `--events`, `--seed`).

## Evidence
- `go test ./scripts/seed` passes (deterministic UUID + .env loading).
- `go run ./scripts/seed` → **17.4 s**: tenants=3, users=15, vendors=300, requisitions=100,000, purchase_orders=66,787, goods_receipts=40,020, invoices=33,159, events=1,000,000.
- `go run ./scripts/seed --reset` → **1.9 s**: demo tables empty and the events collection dropped.
- A second seed run produced identical counts, confirming determinism.
