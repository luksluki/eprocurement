# Identity schema (PostgreSQL)

Owned by the `identity` service per [ADR-0001](../adr/0001-architecture.md): no other service writes to these tables. The schema is created by the embedded migration [`services/identity/migrations/0001_init.sql`](../../services/identity/migrations/0001_init.sql), applied on service boot and in CI (`go run ./services/identity/cmd/migrate`).

## Migration mechanism

- Migrations are `NNNN_name.sql` files under `services/identity/migrations`, applied in ascending version order by `internal/platform/dbmigrate`.
- Each file runs in one transaction together with its record in `identity.schema_migrations` — a failed file leaves no partial state and is retried on the next run.
- Already-recorded versions are skipped, so applying is idempotent.
- A `.sql` file that does not match the naming scheme fails the run instead of being silently skipped.

## Tables

### `identity.users`

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()` |
| `tenant_id` | uuid NOT NULL | Tenant scoping for every query (see *Tenancy* below) |
| `email` | text NOT NULL | Unique per tenant, case-insensitive |
| `display_name` | text NOT NULL default `''` | |
| `password_hash` | text NOT NULL | argon2id in PHC string format (`$argon2id$v=19$m=65536,t=2,p=4$…`); produced by `auth.HashPassword` |
| `status` | text NOT NULL default `'active'` | `active` \| `disabled` |
| `created_at` / `updated_at` | timestamptz NOT NULL | |

Indexes: `users_tenant_email_uidx UNIQUE (tenant_id, lower(email))`; `users_tenant_idx (tenant_id)`.

### `identity.roles`

Fixed RBAC catalog (PRD §7, FR-01), seeded by the migration with `ON CONFLICT DO NOTHING`:

`admin`, `requester`, `approver`, `buyer`, `finance`, `viewer` — `code` is `UNIQUE`.

### `identity.user_roles`

Join table: `user_id` × `role_id` composite PK, both `ON DELETE CASCADE`, `granted_at` default `now()`. A user may hold several roles; the gateway and the service layer both enforce them (defense in depth).

### `identity.refresh_tokens`

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | |
| `user_id` | uuid NOT NULL → `users` | `ON DELETE CASCADE` |
| `token_hash` | bytea NOT NULL UNIQUE | sha256 of the opaque refresh token; the raw value only exists client-side |
| `expires_at` | timestamptz NOT NULL | PRD: 7 days; `CHECK (expires_at > created_at)` |
| `rotated_to` | uuid NULL → `refresh_tokens` | Set when exchanged for a successor (rotation) |
| `revoked_at` | timestamptz NULL | Logout / compromise revocation |
| `created_at` | timestamptz NOT NULL | |

Indexes: `refresh_tokens_user_idx (user_id)`; `refresh_tokens_expires_idx (expires_at)` for cleanup jobs.

## Token semantics (issue #20)

**Access token** — JWT signed with Ed25519 (`alg: EdDSA`), 15 minutes, claims:

| Claim | Meaning |
|---|---|
| `iss` | `identity.eprocurement` |
| `sub` | user id |
| `tid` | tenant id |
| `roles` | granted role codes at issue time |
| `jti` | unique token id |
| `iat` / `exp` | issued-at / expiry (15 min) |

The `kid` header identifies the signing key (first 8 bytes of the public key's sha256). Set `IDENTITY_JWT_KEY` (base64 32-byte seed) for a stable key across restarts; unset means an ephemeral key per boot. The active public keys (current + `IDENTITY_JWT_KEY_PREVIOUS` during rotation) are served to verifiers through the `GetJwks` gRPC RPC; the gateway exposes them at `/.well-known/jwks.json` and verifies tokens against its cached copy. Rotation procedure: [`docs/ops/jwks-rotation.md`](../ops/jwks-rotation.md).

**Refresh token** — 32 random bytes, hex-encoded to the client; only its sha256 is stored (`token_hash`), 7-day expiry. Rules:

- Each `Refresh` call issues a new token and sets the old row's `rotated_to`.
- Presenting a rotated token again (reuse → likely theft) revokes the whole token family via the `rotated_to` chain, then fails.
- `Logout` sets `revoked_at`; revoked and expired tokens always fail with the same generic error.
- Access tokens stay valid until expiry after logout (15 min max exposure); the Redis revocation list for immediate access-token logout is PRD §17, a later issue.

**Login behaviour** — unknown email, wrong password, email present in several tenants (ambiguous), and disabled user all return the identical `invalid email or password` error; the unknown-email path burns an argon2id verification against a dummy hash to keep timing flat. Login attempts are rate-limited per email + client address (`IDENTITY_LOGIN_RATE_PER_MIN`, default 5/min, in-memory).

## Tenancy

`tenant_id` is the scoping key on every user query; the service layer always filters by it, and cross-tenant requests must fail with 404/403 (FR-01). The per-tenant email uniqueness means two tenants may legitimately share an address. PostgreSQL RLS as defense-in-depth (ADR-0001) arrives with the query layer in a later issue, together with cross-tenant denial tests.

## Planned follow-ups

- Gateway REST endpoints for login/refresh/logout (the web shell issue wires them).
- Logout revocation list in Redis (PRD §17) complements `revoked_at` here.
- ListUsers / AssignRoles RPCs — later issues in epic #2.
