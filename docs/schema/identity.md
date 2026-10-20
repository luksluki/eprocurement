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
| `password_hash` | text NOT NULL | argon2id; mandatory from the first user write (login arrives with a later issue) |
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

## Tenancy

`tenant_id` is the scoping key on every user query; the service layer always filters by it, and cross-tenant requests must fail with 404/403 (FR-01). The per-tenant email uniqueness means two tenants may legitimately share an address. PostgreSQL RLS as defense-in-depth (ADR-0001) arrives with the query layer in a later issue, together with cross-tenant denial tests.

## Planned follow-ups

- `identity.v1` gRPC API (Login, Refresh, ListUsers, AssignRoles) — next issues in epic #2.
- JWKS endpoint material: signing keys live in config/secret storage, not in this schema.
- Logout revocation list in Redis (PRD §17) complements `revoked_at` here.
