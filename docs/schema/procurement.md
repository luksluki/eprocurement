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

Indexes: `vendors_tenant_code_uidx UNIQUE (tenant_id, code)`; `vendors_tenant_idx (tenant_id)`; `vendors_tenant_status_idx (tenant_id, status)`.

**RLS**: same pattern as identity — `ENABLE/FORCE ROW LEVEL SECURITY`, policy on `app.tenant_id` with the transaction-scoped `app.rls_bypass` escape used only by auth flows; all vendor queries run through `internal/platform/tenant.WithTenant` as the non-superuser `eproc_app` role. See [identity schema — tenancy](identity.md#tenancy) for the mechanics.

## Lifecycle

- **Create** (`vendor:write`): code normalized to uppercase, validated (`^[A-Z0-9][A-Z0-9-]{1,31}$`); duplicate in the same tenant → `vendor_duplicate_code` (HTTP 409).
- **Read** (`vendor:read`): scoped to the caller's tenant; other tenants' vendors are indistinguishable from missing ones (HTTP 404 `vendor_not_found`).
- **Update** (`vendor:write`): only while active; deactivated vendors are immutable (HTTP 412 `vendor_deactivated`). Empty fields keep stored values.
- **Deactivate** (`vendor:write`): soft delete — status flips, the row stays readable for history. **`GetForUse` is the gate purchase-order creation (#35) must pass: only active vendors may be used in new POs** (HTTP 412 `vendor_deactivated`).

## Error codes

`vendor_code_invalid`, `vendor_name_invalid`, `vendor_email_invalid` → 400 · `vendor_duplicate_code` → 409 · `vendor_not_found` → 404 · `vendor_deactivated` → 412. The gateway maps gRPC codes to HTTP and carries the machine code through in the JSON body (`{"error": "<human>", "code": "<machine>"}`).

## Planned follow-ups

- #26: attachments via presigned uploads.
- #27: list/search/filter/pagination.
- Requisitions, approvals, POs, GRNs, invoices + transactional outbox (M2–M3).
