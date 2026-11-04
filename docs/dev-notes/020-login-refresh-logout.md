# 020 — Login, refresh, logout with argon2id and rotation

- **Issue:** #20
- **Branch:** feature/20-login-refresh-logout
- **Date:** 2026-10-20

## What
- `identity.v1` gRPC API (`Login`, `Refresh`, `Logout`) — `proto/identity/v1/identity.proto` with generated code committed under `gen/go/` (`make gen-proto` regenerates; protoc lives in git-ignored `.tools/`).
- argon2id hashing in PHC string format (`services/identity/auth/password.go`, 64 MiB / t=2 / p=4); `x/crypto` moves from indirect to direct (only new module usage, no new module).
- Access JWT: 15 min, EdDSA (Ed25519) via `internal/platform/jwt` — hand-rolled on `crypto/ed25519` (stdlib-first), `alg` locked to EdDSA, `kid` = sha256-prefix of the public key; claims `iss/sub/tid/roles/jti/iat/exp`. `IDENTITY_JWT_KEY` (base64 seed) keeps the key stable across restarts; unset = ephemeral per boot with a warning.
- Refresh tokens: 32 random bytes hex-encoded client-side, sha256 stored, 7 days. Rotation sets `rotated_to`; reuse of a rotated token revokes the whole family (recursive CTE over the chain) — theft response. `Logout` sets `revoked_at`, idempotent.
- Anti-enumeration: unknown email / wrong password / ambiguous cross-tenant email / disabled user → one identical error; unknown-email burns a dummy argon2id verification for timing parity.
- Login rate limiting: in-memory token bucket per email+peer address (`IDENTITY_LOGIN_RATE_PER_MIN`, default 5/min) → `ResourceExhausted`.
- `cmd/useradmin` CLI creates users with hashes (bootstrap until the gateway/web issues land); compose passes the new env through; `docs/schema/identity.md` documents token semantics.

## Decisions
- **Hand-rolled EdDSA JWT** instead of a JWT library: stdlib-first policy; the slice we need is sign + kid + verify with a single hard-coded algorithm (no algorithm-confusion surface). Flagged for maintainer review in the PR (security-sensitive per docs/ai-workflow.md).
- **Ed25519, not HS256**: ADR-0001/#21 require JWKS at the gateway, which needs asymmetric keys; JWKS exposure itself stays in #21.
- **Login by email requires exactly one match**: the schema allows the same address in several tenants; ambiguous matches fail with the generic invalid-credentials error rather than leaking tenant presence.
- **Family revocation on reuse** over single-token rejection: standard rotation-theft response (OWASP session management), cheap via the `rotated_to` chain.
- Gateway REST exposure is out of scope ("expose login through the gateway only" — identity remains gRPC-internal); #21 brings JWKS + the gateway.

## Evidence
- Unit: jwt (round-trip, wrong key, tampered, expired, HS256-header rejection, malformed), ratelimit (burst/deny, refill, key independence), password (PHC format, salted, malformed-hash rejection).
- Integration (TEST_DATABASE_URL → CI job + local PG): all three acceptance criteria —
  - *401 without enumeration:* `TestLoginFailuresAreIndistinguishable` asserts the exact same error across 5 failure modes; gRPC-level `TestGRPCEndToEndLoginRefreshLogout` asserts identical `Unauthenticated` messages for unknown-email vs wrong-password.
  - *rotation invalidates:* old token rejected after refresh, and the successor is rejected too (family revoked); a 5-link rotation chain works.
  - *logout revokes:* refresh after logout fails; logout idempotent; unknown token logout is a no-op.
  - Plus: rate limit kicks in after N failures per key while another key still works; access JWT parses with correct `tid`/`roles`/`iss`/`jti` and a 15-minute `exp`.
- Full gates: `go vet/build` clean, `go test ./...` green (unit + integration), pnpm install/build/lint/test pass, `docker compose config` valid.
- Compose: image rebuilt, `docker compose up -d --wait` → all services healthy; `grpc-health-probe -service identity` SERVING; `useradmin` CLI created a user with roles (removed afterwards).

## Follow-ups
- #21: JWKS endpoint, gateway token verification, key rotation runbook.
- Redis-based logout revocation for access tokens (PRD §17) and a refresh-token cleanup job for expired rows.
