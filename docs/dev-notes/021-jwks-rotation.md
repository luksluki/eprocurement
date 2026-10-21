# 021 — JWKS endpoint and signing-key rotation

- **Issue:** #21
- **Branch:** feature/21-jwks-rotation
- **Date:** 2026-10-20

## What
- `identity.v1` gains `GetJwks` (proto + regenerated `gen/go`): active public keys (current first) as raw Ed25519 bytes + kid + current flag.
- `auth.Keyring` (current + optional previous key, kids derived from the public keys) replaces the single signing key; login signs with the current key, JWKS serves both during the rotation window. `IDENTITY_JWT_KEY_PREVIOUS` env + compose passthrough.
- **New `gateway` service** (this issue bootstraps it — no gateway existed yet; PRD fixes it as the GoFiber REST edge): `GET /healthz`, public `GET /.well-known/jwks.json` (OKP/Ed25519, `use=sig`, `alg=EdDSA`, base64url `x`), Bearer-token middleware verifying against the cached key set, `GET /api/v1/me` echoing verified claims, JSON request logs with `X-Trace-Id` propagation, graceful shutdown.
- `services/gateway/token`: JWKS cache keyed by kid with TTL (`JWKS_CACHE_TTL`, default 5 min) and **one synchronous refresh on unknown kid** — that is what makes rotation seamless. On fetch failure it serves the cached set rather than failing closed.
- Runbook `docs/ops/jwks-rotation.md`; schema/onboarding/README updated; `GATEWAY_PORT`/`IDENTITY_ADDR` in `.env.example`.

## Decisions
- **GoFiber v2.52.6** (go directive 1.20 — compatible with the repo's Go 1.24) — mandated by the PRD for the REST edge; the only new module.
- JWKS shape conversion (gRPC raw bytes → RFC 7517 OKP) lives in the gateway; identity stays minimal.
- Unknown-kid forced refresh, not TTL-only: a rotation must be honored immediately, while the TTL bounds idle fetches.
- Cache-served-on-fetch-error: a flaky identity should not invalidate valid tokens (keys rotate rarely; a stale key set only delays rotation pickup).
- Business REST routes and login endpoints are deliberately absent — later issues (#22 RBAC, web shell) add them on this skeleton.

## Evidence
- Unit: verifier caching (one fetch, cache hits), rotation (unknown kid → exactly one refresh → verifies; old kid still verifies), rejection matrix (garbage / unknown kid / expired / tampered), KeySet order; fiber handlers (healthz, JWKS shape incl. 32-byte `x`, 401 matrix, /me claims + expired → 401); `GetJwks` serves current-first with and without a previous key (nil-DB service).
- Full gates: `go vet/build` clean, `go test ./...` green incl. integration, pnpm gates pass, `docker compose config` valid.
- **Live compose proof** (both ACs): seeded key A → login → token(kid A) → `GET /api/v1/me` 200. Rotated to key B with A as previous (env + restart): `/.well-known/jwks.json` served 2 keys (current first); **old token (kid A) still 200** after the gateway's unknown-kid refresh; new token (kid B) 200. Cleaned up afterwards (env restored, test user deleted, throwaway login client removed).
- Found and fixed during verification: compose was missing the `IDENTITY_JWT_KEY_PREVIOUS` passthrough (rotation silently degraded to a single key — caught by the live check, not by unit tests) and the gateway healthcheck used `localhost` (IPv6 `::1` inside Alpine) — now `127.0.0.1`.

## Follow-ups
- Login/refresh/logout REST endpoints on the gateway (web shell issue).
- #22 RBAC middleware reuses the claims that `bearerAuth` already stores.
