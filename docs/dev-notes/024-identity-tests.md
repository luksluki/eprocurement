# 024 — Identity unit and integration tests

- **Issue:** #24
- **Branch:** feature/24-identity-tests
- **Date:** 2026-10-21

## What
- **Coverage threshold enforced:** identity domain = `services/identity/{auth,server,config}`; measured at **85.1%** (baseline before this PR: 81.2%), threshold set at **80%**. Enforced by `make cover` locally and a CI step (`go test -coverprofile` + `go tool cover -func` guard) — both need `TEST_DATABASE_URL` so the DB-backed tests count toward coverage.
- **`make test-integration`**: full `go test ./...` against the compose database (`TEST_DATABASE_URL` defaults from `.env`'s `DATABASE_URL`) — the "tests run in compose" AC. CI already ran them via the Postgres service container.
- **New tests:** fixed argon2id PHC vector (parameter-regression guard), login with a corrupted stored hash fails closed with the generic credential error, refresh rejected when the user was disabled after login, config env parsing (defaults, overrides, garbage-rate fallback).
- **De-flaked `TestRefreshExpiredTokenRejected`:** was skipped whenever pgcrypto was unavailable (always, on the compose image — `digest()`); now computes the sha256 in Go and evaluates expiry against an injected clock (`svc.now`), so it always runs and cannot race the wall clock.
- Onboarding documents the unit/integration split and the two new targets.

## Decisions
- Threshold scope is the identity **domain** packages (`cmd` wiring excluded — it is thin composition, expensive to test through `main`).
- 80% threshold: measured 85.1% with a deliberate buffer so routine refactors do not trip it; ratchet it up as coverage grows.
- Time-dependent behaviour is tested with the service's injected `now` (expiry) and the limiter's injected clock — no sleeps anywhere in the suite.

## Evidence
- `make cover` → `OK: identity coverage 85.1% meets the 80% threshold`; `make test-integration` → 12 packages ok against the compose DB.
- Full gates: `go vet ./... && go build ./...` clean; `go test ./...` green with integration; pnpm gates pass; `docker compose config` valid.
- No skipped tests remain in the identity packages (the pgcrypto skip is gone).

## Follow-ups
- Apply the same coverage guard to future domains (procurement M2) when they land.
- Consider a ratchet script (threshold = max(current, +1)) once more services join.
