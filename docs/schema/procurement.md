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

## Migration 0005 — `procurement.approval_rules` / `procurement.approval_steps` (#30)

The storage mirror of the `approval` domain package. Rules are the live
per-tenant configuration; steps are the per-requisition **snapshot** a
submit takes of the rules that matched — replacing rules never rewrites
steps that already exist.

`procurement.approval_rules` (one row per rule; the tenant's whole set is
replaced atomically by the configure API):

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()` |
| `tenant_id` | uuid NOT NULL | RLS scoping key |
| `currency` | text NOT NULL | CHECK `IDR` \| `USD` — a rule only matches a requisition in its own currency |
| `step_no` | integer NOT NULL | CHECK `> 0`; `UNIQUE (tenant_id, currency, step_no)` — position in that currency's chain |
| `role_required` | text NOT NULL | CHECK one of the six fixed roles — deciders must hold it (see below) |
| `threshold_cents` | bigint NOT NULL | CHECK `>= 0`; the rule applies when `total_cents >= threshold_cents` (inclusive lower bound) |
| `created_at`/`updated_at` | timestamptz | |

`procurement.approval_steps` (no `tenant_id` — PRD §13; tenancy derives
from the parent requisition, exactly like `requisition_items`):

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()`; the decide API's `{stepId}` |
| `requisition_id` | uuid NOT NULL | FK → `requisitions (id)` ON DELETE CASCADE |
| `step_no` | integer NOT NULL | CHECK `> 0`; `UNIQUE (requisition_id, step_no)` |
| `role_required` | text NOT NULL | snapshot of the rule's role |
| `threshold_cents` | bigint NOT NULL | snapshot of the rule's threshold |
| `status` | text NOT NULL DEFAULT `pending` | CHECK `pending` \| `approved` \| `rejected` \| `skipped` |
| `decided_by` | uuid NULL | the decider; empty while pending/skipped |
| `decided_at` | timestamptz NULL | |
| `reason` | text NOT NULL DEFAULT '' | required on reject, optional on approve |
| `created_at` | timestamptz NOT NULL | |

Indexes: `approval_rules_tenant_currency_step_uidx UNIQUE (tenant_id,
currency, step_no)`; `approval_rules_tenant_idx (tenant_id)`;
`approval_steps_requisition_stepno_uidx UNIQUE (requisition_id, step_no)`;
`approval_steps_requisition_idx (requisition_id)`.

**RLS**: rules carry `tenant_id` directly (0001 pattern); steps use the
items pattern — the policy derives tenancy from the parent requisition via
`EXISTS`, so cross-tenant step access is indistinguishable from missing
(404 `approval_step_not_found`).

### Approval lifecycle (#30)

- **Rule administration** — `GET`/`PUT /api/v1/approval-rules`
  (`approval:configure`, admin-only by matrix). PUT replaces the tenant's
  whole set in one transaction (delete + insert); the empty set is legal.
  Domain validation: currency ∈ {IDR, USD}, role in the six-role catalog,
  threshold ≥ 0, step_no > 0 and unique per currency.
- **Submit materialization (matrix model)** — submit evaluates **every**
  tenant rule with `currency` matching the requisition and
  `threshold_cents <= total_cents` (inclusive lower bound), not just the
  highest threshold: each match becomes one `pending` step, ordered by
  `step_no`. No applicable rule → `approval_rules_missing` (HTTP 412) and
  the **whole submit transaction rolls back**: the requisition stays a
  draft, no steps exist, and the submit idempotency key is **not** recorded
  (it is written only by the successful UPDATE) — retrying the same key
  after configuring rules behaves as a fresh submit. The pinned replay
  contract for successful submits is unchanged.
- **Steps are in-band** — the submit and decide responses embed the
  requisition's `approval_steps` (ids included). There is no step read
  endpoint yet (#32-34 own timeline/queue); a client chains
  submit → decide → decide purely from responses.
- **Decide** — `POST /api/v1/approvals/{stepId}/decide`
  (`approval:decide`): `{"decision": "approve"|"reject", "reason": "…"}`
  (decision required; reason required on reject after trimming, optional on
  approve, ≤ 1000 chars at the edge and the service layer). Only the
  **current** step — the smallest pending `step_no` of a requisition in
  `submitted` — may be decided; anything else → 409
  `approval_step_not_current`. Authorization beyond the permission: the
  decider must not be the requester (`approval_self_forbidden`, **no admin
  exception**) and must hold the step's `role_required`
  (`approval_role_forbidden`; admins excepted, mirroring `rbac.Check` and
  the requisition ownership rule). Because `approval:decide` is
  approver-only in the matrix, **a rule with role R can only be decided by
  a user who holds both R and `approval:decide` (i.e. R + approver), or by
  an admin.** A reject marks the step `rejected`, every later pending step
  `skipped`, and the requisition `rejected` (terminal); approving the last
  pending step approves the requisition. Decisions serialize on the parent
  requisition row `FOR UPDATE` inside the same `tenant.WithTenant`
  transaction as submit/edit — concurrent decides on one step yield exactly
  one winner, the others 409.
- **Events** — every step decision records `approval_step_approved` /
  `approval_step_rejected` on the step aggregate's in-memory log; the
  terminal transitions reuse the existing `requisition_approved` /
  `requisition_rejected`. Since #31 the persisted record is the outbox rows
  Decide writes in the same transaction (see 0006 below) — with the
  publishable two-segment names of the PRD §14.6 catalog, not these
  aggregate-local names.
- **Decide idempotency (#31)** — the optional `Idempotency-Key` header
  (1–64 chars after trimming, else 400 `approval_idempotency_key_invalid`
  at the gateway and the service layer; blank after trimming = a plain
  request) names one decision, with the contract pinned like the submit
  key's: replaying the same key on the same step returns the requisition's
  current state (plus its steps) **without a second decision, a second
  outbox row, or a 409**; reusing the key for another step of the tenant →
  409 `approval_idempotency_key_conflict`; a duplicate decide **without** a
  key on an already-decided step keeps the #30 answer
  (`approval_step_not_current`). The replay lookup and the key insert run
  inside the decide transaction after the parent `FOR UPDATE` lock, and the
  key is recorded on the success path only — two concurrent keyed decides
  on one step serialize into one winner + one idempotent replay, never a
  conflict.

## Migration 0006 — `procurement.outbox` + `procurement.decision_idempotency` (#31)

The persistent half of the transactional outbox (PRD §14.3, ADR-0001):
state writes and their events commit atomically in one transaction; the
publisher loop that drains the table to RabbitMQ (poll 2 s, batch 50,
`published_at` marker, retries, DLQ) stays with #45. `outbox.AppendTx`
(`services/procurement/outbox`) is the writer; Decide is its first caller.

`procurement.outbox`:

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | **= `event_id`** of the §14.5 envelope — consumers dedupe on it |
| `tenant_id` | uuid NOT NULL | RLS scoping key |
| `event_type` | text NOT NULL | catalog names, e.g. `pr.step_approved` |
| `version` | integer NOT NULL DEFAULT 1 | **delta vs §13**: the envelope's schema `version` (§14.5) as a column, so the #45 publisher assembles envelopes without parsing the payload |
| `payload` | jsonb NOT NULL | decision payload — see below |
| `actor_id` | uuid NULL | **delta vs §13**: envelope `actor_id` (§14.5) as a column; the decider |
| `trace_id` | text NOT NULL DEFAULT '' | **delta vs §13**: envelope `trace_id` (§14.5) as a column; the propagated REST `X-Trace-Id` |
| `entity_type` / `entity_id` | text / uuid NOT NULL | **delta vs §13/§14.5**: filter keys for `GET /api/v1/events?entityType=&entityId=` (§15) and the timeline projection (#43); for decide events the entity is the **requisition**, the step rides in the payload |
| `occurred_at` | timestamptz NOT NULL | **delta vs §13**: envelope `occurred_at` (§14.5) as a column |
| `created_at` | timestamptz NOT NULL | |
| `published_at` | timestamptz NULL | set by the #45 publisher; rows are never deleted |

These `version`/`actor_id`/`trace_id`/`occurred_at`/`entity_*` columns are a
**conscious schema delta** beyond the §13 summary row and the §14.5 envelope
— recorded here rather than claimed as PRD-conformant. Indexes:
`outbox_unpublished_idx (created_at) WHERE published_at IS NULL` — the #45
poller's partial index. **RLS**: the 0001/0003 tenant pattern; the #45
publisher reads through the transaction-scoped `app.rls_bypass` arm.

**Event naming (pinned for #31).** PRD §14.5 binds the topic exchange with
`pr.*`, and the `*` wildcard matches exactly **one** word — so every
procurement event name must stay two-segment to be deliverable. The decide
events are:

- `pr.step_approved` / `pr.step_rejected` — one per decision (step event).
  **Catalog extension**: not in the §14.6 list; a three-segment step name
  would never match a natural `pr.*` binding.
- `pr.approved` / `pr.rejected` — only when the flow finishes (the §14.6
  catalog entries).

A mid-flow approve writes exactly one row; a decision that completes or
kills the flow writes two (step + terminal). Replays, key conflicts, and
duplicates without a key write nothing.

**Decision payload** (both events share it): `requisition_id`, `number`,
`requester_id` (the notification recipient, #45), `step_id`, `step_no`,
`decision`, `reason`, `decided_by` (also the `actor_id` column),
`decided_at`.

`procurement.decision_idempotency` — one row per recorded decide key,
inserted inside the decide transaction **on the success path only**:
`tenant_id` + `idempotency_key` composite PK, `step_id` FK →
`approval_steps` `ON DELETE CASCADE`, `created_at`. A dedicated table (not a
key column on `approval_steps`) because the key names a request whose
uniqueness scope is the tenant, and `approval_steps` deliberately carries no
`tenant_id` (0005) — a unique key column there would let a cross-tenant
23505 leak which keys exist in other tenants. **RLS**: the 0001 tenant
pattern on `tenant_id`.

## Requisition state machine (#28)

Seven legal edges (loose cancel — maintainer decision on #28):
`draft → submitted`, `draft → cancelled`, `submitted → approved`,
`submitted → rejected`, `submitted → cancelled`, `approved → cancelled`,
`approved → closed`. `rejected`, `cancelled`, `closed` are terminal.
`approved → closed` is driven by PO conversion (#35); the domain `Close`
method exists now. Every successful transition appends exactly one domain
event (`requisition_submitted`, …, plus `requisition_created` on draft
creation) to the in-memory aggregate log (`Events()` / `PullEvents()`). The
decide events are persisted to `procurement.outbox` since #31 (0006);
submit/create events still live only in the in-memory log until #45 wires
them.

Error codes: `requisition_currency_invalid`, `requisition_description_invalid`,
`requisition_qty_invalid`, `requisition_unit_price_invalid`,
`requisition_items_empty`, `requisition_id_invalid`,
`requisition_idempotency_key_invalid` → 400 · `requisition_illegal_transition` → 409
· `requisition_idempotency_key_conflict` → 409 · `requisition_not_found` → 404 ·
`requisition_forbidden` → 403 (domain/store sentinel → gRPC code in the
procurement server; the gateway maps `Aborted` → HTTP 409 — live since #29) ·
`approval_rules_missing` → 412 (#30: submit found no applicable rule and rolled
back).

Approval error codes (#30): `approval_rule_currency_invalid`,
`approval_rule_role_invalid`, `approval_rule_threshold_invalid`,
`approval_rule_step_no_invalid`, `approval_rule_step_no_duplicate`,
`approval_decision_invalid`, `approval_reason_required`,
`approval_reason_invalid` (reason over 1000 chars),
`approval_step_id_invalid`, `approval_idempotency_key_invalid` (key over
64 chars) → 400 · `approval_step_not_current` → 409 ·
`approval_idempotency_key_conflict` → 409 (#31) ·
`approval_step_not_found` → 404 (also the cross-tenant answer, via the
transitive RLS policy) · `approval_self_forbidden`,
`approval_role_forbidden` → 403 · `approval_rules_missing` → 412. The
gateway carries them through the shared gRPC→HTTP mapping
(`AlreadyExists` → 409 for the key conflict).

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
  `requisition_items_empty`, invalid line → the line's 400 code) **and at
  least one applicable approval rule must exist** (none → 412
  `approval_rules_missing`, whole submit rolled back — see the approval
  lifecycle above). The optional `Idempotency-Key` header (1–64 chars after
  trimming, else 400 `requisition_idempotency_key_invalid`) names the
  submit: replaying the same key **returns the requisition's current state
  as-is** (no second transition, no 409), whatever it has moved on to
  since — a contract pinned for #30/#31. Reusing the key for another
  requisition in the tenant → 409 `requisition_idempotency_key_conflict`.
  The response embeds the materialized `approval_steps` (ids in-band).

## Error codes

`vendor_code_invalid`, `vendor_name_invalid`, `vendor_email_invalid` → 400 · `vendor_duplicate_code` → 409 · `vendor_not_found` → 404 · `vendor_deactivated` → 412. The gateway maps gRPC codes to HTTP and carries the machine code through in the JSON body (`{"error": "<human>", "code": "<machine>"}`) — the same shared mapping serves vendors and requisitions (`InvalidArgument`→400, `NotFound`→404, `AlreadyExists`/`Aborted`→409, `FailedPrecondition`→412, `PermissionDenied`→403).

## Planned follow-ups

- #26: attachments via presigned uploads (done).
- #27: list/search/filter/pagination (done).
- #28: requisition domain + state machine + tables (done).
- #29: requisition create/edit/submit API (done — store, number generation, idempotent submit, REST/gRPC wiring, gateway `Aborted` → 409). Read endpoints (get/list/timeline) are separate issues.
- #30: approval rules + sequential engine + decide API (done — matrix materialization, 412 rollback, self/role enforcement, reject-skips, admin rule configuration, steps in-band).
- #31: idempotent decide + outbox audit/notification events (done — `procurement.outbox` + `decision_idempotency` in 0006, decide replay/conflict contract, `pr.*` decide events with actor + trace, gateway `x-trace-id` propagation). **Re-sequenced with #45** by an agent decision: the outbox table + writer moved from #45's scope into #31 (see dev-note 031's provenance block); #45 keeps the publisher loop only.
- Approvals read endpoints/queue #32-34, PO conversion (#35), outbox publisher (#45 — poll/publish/retry/DLQ only; table and writer landed with #31) (M2–M3).
- The timeline/event read path (#42/#43/#47, `GET /api/v1/events`) consumes the outbox's `entity_type`/`entity_id` columns; a tenant+entity index arrives with whichever issue first queries them.

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
