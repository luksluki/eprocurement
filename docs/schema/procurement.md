# Procurement schema (PostgreSQL)

Owned by the `procurement` service per [ADR-0001](../adr/0001-architecture.md). Created by the embedded migrations in [`services/procurement/migrations/`](../../services/procurement/migrations/), applied on service boot and in CI (`go run ./services/procurement/cmd/migrate`). Same migration mechanics as identity (per-file transactions, `schema_migrations` tracking, idempotent).

## Migration 0001 — `procurement.vendors`

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()` |
| `tenant_id` | uuid NOT NULL | RLS scoping key |
| `code` | text NOT NULL | canonical uppercase; `UNIQUE (tenant_id, code)` — codes are unique per tenant only |
| `name` | text NOT NULL | 2–200 chars |
| `contact_email` | text | optional, format-checked |
| `tax_id` | text | optional |
| `status` | text | `active` \| `deactivated` — soft delete, rows are never removed |
| `created_at`/`created_by`, `updated_at`/`updated_by` | timestamptz / uuid | audit fields; `*_by` = the acting user from the propagated auth context |

Indexes: `vendors_tenant_code_uidx UNIQUE (tenant_id, code)`; `vendors_tenant_idx (tenant_id)`; `vendors_tenant_status_code_idx (tenant_id, status, code)`; `vendors_name_trgm_idx GIN (name gin_trgm_ops)` — see the list section below.

**RLS**: same pattern as identity — `ENABLE/FORCE ROW LEVEL SECURITY`, policy on `app.tenant_id` with the transaction-scoped `app.rls_bypass` escape used only by auth flows; all vendor queries run through `internal/platform/tenant.WithTenant` as the non-superuser `eproc_app` role. See [identity schema — tenancy](identity.md#tenancy) for the mechanics.

## Lifecycle

- **Create** (`vendor:write`): code normalized to uppercase, validated (`^[A-Z0-9][A-Z0-9-]{1,31}$`); duplicate in the same tenant → `vendor_duplicate_code` (HTTP 409).
- **Read** (`vendor:read`): scoped to the caller's tenant; other tenants' vendors are indistinguishable from missing ones (HTTP 404 `vendor_not_found`).
- **Update** (`vendor:write`): only while active; deactivated vendors are immutable (HTTP 412 `vendor_deactivated`). Empty fields keep stored values.
- **Deactivate** (`vendor:write`): soft delete — status flips, the row stays readable for history. **`GetForUse` is the gate purchase-order creation (#35) must pass: only active vendors may be used in new POs** (HTTP 412 `vendor_deactivated`).

## Error codes

`vendor_code_invalid`, `vendor_name_invalid`, `vendor_email_invalid` → 400 · `vendor_duplicate_code` → 409 · `vendor_not_found` → 404 · `vendor_deactivated` → 412. The gateway maps gRPC codes to HTTP and carries the machine code through in the JSON body (`{"error": "<human>", "code": "<machine>"}`).

## Planned follow-ups

- #26: attachments via presigned uploads (done).
- #27: list/search/filter/pagination (done).
- Requisitions, approvals, POs, GRNs, invoices + transactional outbox (M2–M3).

## Vendor list — filters, pagination, query plans (#27)

`ListVendors` (`vendor:read`, REST `GET /api/v1/vendors`) returns one page
ordered by `code` plus the total number of matching rows:

- `q` — code prefix **or** name substring, case-insensitive. LIKE wildcards
  in the input (`%`, `_`, `\`) are escaped, so they match literally.
- `status` — `active` | `deactivated` | empty (all); combines with `q`.
- `page` (1-based, default 1) / `page_size` (default 20, max 100). Responses
  echo the normalized values; `total` counts matches across all pages. A
  page past the end is empty but still reports the true `total` (an
  explicit COUNT fallback covers the case where the window function emits
  no rows).

### Indexes (migration 0002)

- `vendors_tenant_status_code_idx (tenant_id, status, code)` — replaces
  0001's `(tenant_id, status)`; serves status-filtered, code-ordered pages.
- `vendors_name_trgm_idx GIN (name gin_trgm_ops)` (`pg_trgm`) — name
  substring search.
- Code prefixes use the existing `vendors_tenant_code_uidx (tenant_id, code)`.

### Measured plans at 100k rows (the #27 budget test)

`TestStoreListScale100k` seeds 100,000 vendors in one tenant (10% with a
shared name word, 10% deactivated), ANALYZEs, then runs every filter shape
20× under the real RLS context. Budget: **p95 < 300 ms** per query,
mirroring the PRD's performance bar. Measured on the dev machine:

| Shape | median | p95 |
|---|---|---|
| unfiltered page 1 | ~73 ms | ~78 ms |
| status = deactivated (10% of rows) | ~15 ms | ~16 ms |
| code prefix (~1k rows) | ~57 ms | ~62 ms |
| name substring (~10k rows) | ~63 ms | ~66 ms |
| status + name substring combined | ~61 ms | ~74 ms |

All shapes pass with >4× headroom. Two facts from the EXPLAIN output
(captured in the test log) are worth knowing:

1. **The planner prefers (parallel) sequential scans at this scale — by
   design, not by accident.** `total` rides along as `count(*) OVER()`, so
   every matching row is visited no matter how small the page; and reading
   the ~1,500-page heap is cheaper than index+heap lookups at 100k rows.
   The same plans appear even without RLS. The btree/trigram indexes above
   exist for the shapes where they pay off: deeper pages, more selective
   prefixes, and scales where the heap stops fitting in cache.
2. **The RLS policy's bypass arm keeps tenant-leading index quals
   unavailable.** `tenant_id = current_setting(...) OR app.rls_bypass='on'`
   cannot be planned as an index constraint (the planner cannot prove
   tenant selectivity), so tenant scoping always executes as a filter.
   That is the documented trade-off of #23's single-policy design; if list
   latency ever matters more than the shared policy, the follow-up is a
   per-service read role or policy reshaping — a platform decision, not a
   vendor-list tweak.
