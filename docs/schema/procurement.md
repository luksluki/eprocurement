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

## Migration 0003 — `procurement.requisitions` / `procurement.requisition_items` (#28)

The storage mirror of the `requisition` domain package (state machine, events,
validation live in `services/procurement/requisition`).

`procurement.requisitions`:

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()` |
| `tenant_id` | uuid NOT NULL | RLS scoping key |
| `number` | text NOT NULL | `UNIQUE (tenant_id, number)` — numbers unique per tenant only; server-generated per tenant since #29 (see 0004) |
| `requester_id` | uuid NOT NULL | the user who created the PR |
| `status` | text NOT NULL DEFAULT `draft` | CHECK: `draft` \| `submitted` \| `approved` \| `rejected` \| `cancelled` \| `closed` — mirrors the domain state machine as defense in depth |
| `currency` | text NOT NULL | CHECK: `IDR` \| `USD` — mirrors the domain allowlist (FR-03) |
| `total_cents` | bigint NOT NULL DEFAULT 0 | sum of `qty × unit_price_cents`, computed server-side only |
| `submitted_at` | timestamptz NULL | set on draft → submitted |
| `submit_idempotency_key` | text NULL | added by 0004; one key names one submit per tenant |
| `created_at`/`created_by`, `updated_at`/`updated_by` | timestamptz / uuid | audit fields, same shape as vendors |

`procurement.requisition_items` (no `tenant_id` — PRD §13; tenancy derives
from the parent):

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()` |
| `requisition_id` | uuid NOT NULL | FK → `requisitions (id)` ON DELETE CASCADE |
| `description` | text NOT NULL | non-blank enforced in the domain |
| `qty` | integer NOT NULL | CHECK `qty > 0` |
| `unit_price_cents` | bigint NOT NULL | CHECK `unit_price_cents >= 0`; integer cents in the header currency |
| `category` | text NOT NULL DEFAULT '' | free-form |
| `created_at` | timestamptz NOT NULL | |

Indexes: `requisitions_tenant_number_uidx UNIQUE (tenant_id, number)`;
`requisitions_tenant_idx (tenant_id)`; `requisitions_tenant_status_idx
(tenant_id, status)`; `requisition_items_requisition_idx (requisition_id)`.

**RLS**: `ENABLE/FORCE` + `tenant_isolation` on both tables, exactly the 0001
pattern — except the items policy derives tenancy from the parent
requisition (`EXISTS` subquery), so isolation stays transitive without
denormalizing `tenant_id` onto the lines. Cross-tenant reads, updates,
deletes, and item inserts under another tenant's requisition are all denied
(verified against a live PostgreSQL).

## Migration 0004 — `procurement.requisition_counters` + submit idempotency (#29)

`procurement.requisition_counters` (one row per tenant — the serialization
point for number generation):

| Column | Type | Notes |
|---|---|---|
| `tenant_id` | uuid PK | RLS scoping key |
| `last_number` | bigint NOT NULL DEFAULT 0 | last allocated PR number |

Number generation: `Create` runs the counter init + increment + requisition
insert in one transaction. The row is initialized lazily from the tenant's
highest existing `PR-%06d` number (`INSERT … SELECT max(…) ON CONFLICT DO
NOTHING`; once a counter exists it is authoritative), then
`UPDATE … SET last_number = last_number + 1 RETURNING` takes the row lock —
concurrent creates in one tenant serialize and receive distinct sequential
numbers (demonstrated by a parallel store test). A failed create rolls the
increment back, so numbers have no gaps from validation errors.

Also in 0004: `requisitions.submit_idempotency_key text` plus the partial
unique index `requisitions_tenant_submit_key_uidx (tenant_id,
submit_idempotency_key) WHERE submit_idempotency_key IS NOT NULL` — one key
names one submit per tenant. Reusing a key for a different requisition's
submit fails with 23505 and maps to `requisition_idempotency_key_conflict`
(HTTP 409).

**RLS** on the counters: the 0001/0003 pattern (`ENABLE/FORCE` +
`tenant_isolation` + grants). 0001's `ALTER DEFAULT PRIVILEGES` already
covers tables created later; the explicit `GRANT SELECT, INSERT, UPDATE` is
the same belt-and-braces as 0003.

## Requisition state machine (#28)

Seven legal edges (loose cancel — maintainer decision on #28):
`draft → submitted`, `draft → cancelled`, `submitted → approved`,
`submitted → rejected`, `submitted → cancelled`, `approved → cancelled`,
`approved → closed`. `rejected`, `cancelled`, `closed` are terminal.
`approved → closed` is driven by PO conversion (#35); the domain `Close`
method exists now. Every successful transition appends exactly one domain
event (`requisition_submitted`, …, plus `requisition_created` on draft
creation) to the in-memory aggregate log (`Events()` / `PullEvents()`) —
the outbox writer comes with #45.

Error codes: `requisition_currency_invalid`, `requisition_description_invalid`,
`requisition_qty_invalid`, `requisition_unit_price_invalid`,
`requisition_items_empty`, `requisition_id_invalid`,
`requisition_idempotency_key_invalid` → 400 · `requisition_illegal_transition` → 409
· `requisition_idempotency_key_conflict` → 409 · `requisition_not_found` → 404 ·
`requisition_forbidden` → 403 (domain/store sentinel → gRPC code in the
procurement server; the gateway maps `Aborted` → HTTP 409 — live since #29).

## Lifecycle

- **Create** (`vendor:write`): code normalized to uppercase, validated (`^[A-Z0-9][A-Z0-9-]{1,31}$`); duplicate in the same tenant → `vendor_duplicate_code` (HTTP 409).
- **Read** (`vendor:read`): scoped to the caller's tenant; other tenants' vendors are indistinguishable from missing ones (HTTP 404 `vendor_not_found`).
- **Update** (`vendor:write`): only while active; deactivated vendors are immutable (HTTP 412 `vendor_deactivated`). Empty fields keep stored values.
- **Deactivate** (`vendor:write`): soft delete — status flips, the row stays readable for history. **`GetForUse` is the gate purchase-order creation (#35) must pass: only active vendors may be used** (HTTP 412 `vendor_deactivated`).

### Requisition lifecycle (#29)

All three endpoints require `requisition:write` (requester/admin) at the
gateway **and** the procurement interceptor; edit/submit additionally enforce
requester ownership — the caller must be `requester_id`, admins excepted
(maintainer decision 2026-10-23) — violating it returns 403
`requisition_forbidden`. Cross-tenant access is indistinguishable from
missing (404 `requisition_not_found`).

- **Create** — `POST /api/v1/requisitions`: the caller becomes the requester.
  The number (`PR-%06d`) is generated server-side per tenant; the total is
  computed server-side and echoed back (the client payload has no total
  field). Currency is required — normalized (trim + uppercase) at the edge,
  matched exactly against `{IDR, USD}`, no default. Items may be empty on a
  draft.
- **Edit** — `PUT /api/v1/requisitions/{id}`: replaces the whole item basket
  of a draft and recomputes the total. Editing a non-draft → 409
  `requisition_illegal_transition` (submit and edit on the same requisition
  serialize on the row lock; the mutation also guards on `status = 'draft'`).
- **Submit** — `POST /api/v1/requisitions/{id}/submit`: draft → submitted;
  the basket must hold at least one valid line (empty → 400
  `requisition_items_empty`, invalid line → the line's 400 code). The
  optional `Idempotency-Key` header (1–64 chars after trimming, else 400
  `requisition_idempotency_key_invalid`) names the submit: replaying the same
  key **returns the requisition's current state as-is** (no second
  transition, no 409), whatever it has moved on to since — a contract pinned
  for #30/#31, when submitted requisitions can advance further. Reusing the
  key for another requisition in the tenant → 409
  `requisition_idempotency_key_conflict`.

## Error codes

`vendor_code_invalid`, `vendor_name_invalid`, `vendor_email_invalid` → 400 · `vendor_duplicate_code` → 409 · `vendor_not_found` → 404 · `vendor_deactivated` → 412. The gateway maps gRPC codes to HTTP and carries the machine code through in the JSON body (`{"error": "<human>", "code": "<machine>"}`) — the same shared mapping serves vendors and requisitions (`InvalidArgument`→400, `NotFound`→404, `AlreadyExists`/`Aborted`→409, `FailedPrecondition`→412, `PermissionDenied`→403).

## Planned follow-ups

- #26: attachments via presigned uploads (done).
- #27: list/search/filter/pagination (done).
- #28: requisition domain + state machine + tables (done).
- #29: requisition create/edit/submit API (done — store, number generation, idempotent submit, REST/gRPC wiring, gateway `Aborted` → 409). Read endpoints (get/list/timeline) are separate issues.
- Approvals (#30/#31), PO conversion (#35), outbox (#45) (M2–M3).

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
