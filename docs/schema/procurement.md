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
state writes and their events commit atomically in one transaction.
`outbox.AppendTx` (`services/procurement/outbox`) is the writer; Decide
(#31) and Submit (#45, `pr.submitted`) are its callers. The publisher loop
that drains the table to RabbitMQ landed with #45 — see the publisher
section below.

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

### Outbox publisher (#45)

The worker's publisher loop (`services/worker/publisher`) drains the table
to RabbitMQ per PRD §16: poll every 2 s (batch 50 — env overrides
`WORKER_POLL_INTERVAL`/`WORKER_BATCH_SIZE` exist for tests only), oldest
first, publish, mark. The read/write side (`outbox.Pending` /
`outbox.MarkPublished`, `services/procurement/outbox/pending.go`) lives with
the table's owning service and is executed by the worker — a conscious
cross-service import inside the single Go module.

- **Cross-tenant read on purpose.** Pending and MarkPublished run through
  the transaction-scoped `app.rls_bypass` arm because one batch drains every
  tenant's rows. This is the deliberate, documented inverse of the
  "every query is tenant-scoped" convention — writer-side RLS stays
  enforced (AppendTx only runs inside `WithTenant`; a tenant-scoped read
  still sees only its own rows, verified by test).
- **Envelope.** The PRD §14.5 envelope `{event_id, tenant_id, type,
  version, occurred_at, actor_id, trace_id, payload}` is assembled 1:1 from
  the row columns — no payload parsing. It is published as JSON to the
  durable topic exchange `procurement.events` with routing key = event
  type, `delivery_mode` persistent, on a confirm-mode channel; the loop
  waits for the broker confirm before marking `published_at`.
- **At-least-once.** A crash between publish and mark republishes the row
  next tick; consumers dedupe on `event_id` (§14.8). A publish failure
  (broker down, confirm timeout) or a poll failure (database restarting,
  migrations not yet applied on a fresh volume) is non-fatal: warn log with
  the event's `trace_id`, next tick retries. The worker deliberately runs
  no migrations — procurement stays the sole applier — so its `depends_on`
  in compose is only postgres + rabbitmq healthy.
- **RabbitMQ topology (#46).** Connecting declares the full §14.5 topology
  (next section) — the exchanges, the three consumer queues with their DLX
  argument and TTL retry ladders, and the DLQ — replacing #45's PROVISIONAL
  bootstrap. The `procurement.dlx`/`procurement.dlq` declarations stayed
  argument-identical, so redeclaring on a broker that carries the #45 pair
  cannot raise PRECONDITION_FAILED.
- **Observability (#54 follow-up).** What exists now is the structured
  publish log (`event_id`, `tenant_id`, `event_type`, `trace_id` from the
  row). OTel spans, trace propagation into AMQP headers, and the outbox
  lag metric (§18) are deferred to #54, which introduces the observability
  stack (compose.yaml's profile note); the repo has no OTel/Prometheus code
  yet.

**`pr.submitted` payload (#45, pinned public contract)** — written by the
Submit transaction (`requisition/store.go` appendSubmittedTx): the same
basket of consumers as the decide events reads it (timeline #43;
notifications reads `requester_id` as the recipient). snake_case follows
the DecisionPayload convention: `requisition_id`, `number`, `requester_id`,
`currency`, `total_cents`, `submitted_by`, `submitted_at`. One row on a
successful submit; zero on an idempotent replay or any rollback (missing
approval rules, empty basket, illegal transition).

**Numbering mismatch, flagged:** `approval/store.go`'s DecisionPayload
comment and dev-note 031 call the notification consumer "the #45 worker";
issue #45's own scope text covers the publisher only. The consumers remain
future issues — the references are historical, kept as-is.

### RabbitMQ topology (#46)

Declared once, idempotently, on every publisher connect (boot and
reconnect) by `services/worker/topology`; the #47 consumers reuse the same
declaration. It replaces #45's PROVISIONAL DLX bootstrap — the
`procurement.dlx`/`procurement.dlq` declarations are argument-identical to
that bootstrap, so the handover cannot raise PRECONDITION_FAILED on a
broker that already carries it.

- **Exchanges.** `procurement.events` (topic, durable) — the publisher's
  target, routing key = event type (§14.6); `procurement.dlx` (topic,
  durable) — the dead-letter destination.
- **Main queues** (durable, `x-dead-letter-exchange: procurement.dlx`):
  `documents.history` bound `#` (the history projection takes the whole
  catalog), `notifications` bound the actor-facing subset `pr.submitted`,
  `pr.approved`, `pr.rejected`, `po.issued`, `po.acknowledged`,
  `invoice.discrepancy`, and `exports` bound `export.requested`. This
  binding table is the repo's routing convention; the notifications subset
  is the one judgement call in it, documented in dev-note 046 and open to
  maintainer override.
- **Retry ladder.** `<queue>.retry.1/2/3` per main queue — durable,
  `x-message-ttl` 5 s → 30 s → 5 min, empty `x-dead-letter-exchange` (the
  default exchange) and `x-dead-letter-routing-key` = the main queue's
  name: a parked message returns to its own queue only, so one queue's
  retries can never land in another's. Nothing consumes the tiers; the TTL
  does the waiting, the broker does the dead-lettering back.
- **Failure path.** Consumers (#47) track attempts in the `x-retry-count`
  header through `topology.NextRetry` / `topology.RetryCount`: failures
  1–3 republish to the next tier with a bumped count; the 4th failure
  rejects without requeue, the queue's DLX argument routes it to
  `procurement.dlx`, and the `#`-bound `procurement.dlq` is the final,
  admin-visible destination (§14.5 "after max attempts → DLQ").

## Migration 0007 — `procurement.purchase_orders` / `po_items` / `po_counters` (#35)

The storage mirror of the `purchaseorder` domain package: the draft PO that
conversion (#35) creates from an approved requisition. The lifecycle
transitions out of `draft` (issue → acknowledged → … → closed/cancelled,
FR-06) landed with #36 and only move `status` / `issued_at` here — see the
[PO lifecycle state machine](#po-lifecycle-state-machine-36) section.

`procurement.purchase_orders`:

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()` |
| `tenant_id` | uuid NOT NULL | RLS scoping key |
| `number` | text NOT NULL | `UNIQUE (tenant_id, number)` — `PO-%06d`, allocated per tenant |
| `requisition_id` | uuid NOT NULL | FK → `requisitions` (no cascade — a converted requisition must not be deletable); **`UNIQUE (tenant_id, requisition_id)` = "cannot convert twice" as a database guarantee** |
| `vendor_id` | uuid NOT NULL | FK → `vendors`; chosen at conversion (requisitions carry no vendor, PRD §13) |
| `status` | text NOT NULL DEFAULT `draft` | CHECK: all **seven** FR-06 statuses (`draft`, `issued`, `acknowledged`, `partially_received`, `received`, `closed`, `cancelled`) from day one, so #36 never needs an ALTER CHECK; #35 writes `draft` only |
| `currency` | text NOT NULL | CHECK `IDR` \| `USD` — **delta vs §13**: the requisition's currency, copied verbatim (every money field needs its unit) |
| `total_cents` | bigint NOT NULL DEFAULT 0 | §13's `total` as integer cents; recomputed server-side from the copied lines (= the requisition's total) |
| `issued_at` | timestamptz NULL | NULL while draft; #36 sets it on issue |
| `created_at`/`created_by`, `updated_at`/`updated_by` | timestamptz / uuid | audit fields, same shape as vendors/requisitions (**delta vs §13**) |

`procurement.po_items` (no `tenant_id` — PRD §13; tenancy derives from the
parent PO, exactly like `requisition_items`):

| Column | Type | Notes |
|---|---|---|
| `id` | uuid PK | `gen_random_uuid()` |
| `po_id` | uuid NOT NULL | FK → `purchase_orders` `ON DELETE CASCADE` |
| `description` | text NOT NULL | copied verbatim from the requisition line |
| `qty_ordered` | integer NOT NULL | CHECK `> 0`; copied verbatim |
| `qty_received` | integer NOT NULL DEFAULT 0 | CHECK `>= 0`; 0 on creation, advanced by goods receipts (#37) |
| `unit_price_cents` | bigint NOT NULL | CHECK `>= 0`; §13's `unit_price` as integer cents, copied verbatim |
| `created_at` | timestamptz NOT NULL | |

The requisition line's `category` is not carried — §13's po_items has no
category column.

`procurement.po_counters` — one row per tenant (`tenant_id` PK,
`last_number`), the exact 0004 pattern. **A counter table, not a native
PostgreSQL SEQUENCE**: `nextval()` is not transactional — it never rolls
back — so a conversion that fails after allocation (vendor deactivated,
requisition not approved) would burn a number and leave a gap in the
per-tenant sequence (FR-05). `UPDATE … RETURNING` under the counter row lock
rolls the increment back with the transaction, keeping numbers gap-free.
**RLS**: the 0001/0004 tenant pattern on all three tables (items derive
tenancy from the parent PO via `EXISTS`, like `requisition_items`).

### PO creation — the conversion contract (#35)

`POST /api/v1/purchase-orders/from-requisition/{id}` (`po:write`, buyer/admin
at both layers; PRD §15) with `{"vendor_id": "<uuid>"}` — **vendor_id is
required**: the requisition carries no vendor (PRD §13). The whole flow is
one tenant transaction whose step order is pinned
(`purchaseorder.Store.CreateFromRequisition`):

1. **Lock** the requisition row `FOR UPDATE` (`requisition.LoadForUpdate`) —
   the serialization point shared with submit/edit/decide.
2. **Replay lookup** by `(tenant_id, requisition_id)`: a PO that already
   exists is returned **as-is** with `created=false` — HTTP **200** (201 only
   when this call created the PO), the vendor gate skipped, a differing
   `vendor_id` ignored, no second write, no second outbox row.
   This includes replays after the vendor has since been deactivated: the
   gate runs on the creation path only.
3. **Vendor gate inside the transaction** — `vendor.ForUseTx(ctx, tx, id)`,
   the tx-scoped twin of #25's pool-based `GetForUse` (which opens its own
   transaction and cannot run inside the caller's): only an **active** vendor
   of the tenant converts, and it cannot be deactivated between the check
   and the insert (anti-TOCTOU). Deactivated → 412 `vendor_deactivated`, the
   whole transaction rolls back (requisition stays approved, no number
   burned).
4. **Status gate**: only an `approved` requisition converts — anything else
   → 409 `po_requisition_not_approved`.
5. **Number allocation**: `PO-%06d` from `po_counters` (lazy init + UPDATE …
   RETURNING under the row lock) **after** the gates, so failures roll the
   increment back.
6. **Guarded close**: the requisition moves `approved → closed` — the domain
   edge #28's state machine reserved for this flow (`Close`), re-checked by a
   `status = 'approved'`-guarded UPDATE.
7. **Insert** the PO + copy the requisition lines verbatim (`qty_received`
   starts 0); `total_cents` is recomputed server-side and must equal the
   requisition's total (asserted).
8. **Outbox**: exactly one `po.created` row commits atomically with the PO
   (below).

Concurrent conversions of one requisition serialize on the requisition lock:
one creates, the other replays the winner's PO — the
`(tenant_id, requisition_id)` unique index is the belt-and-braces backstop.
**Gap-free numbering evidence lives in the store test suite**
(`services/procurement/purchaseorder/store_test.go`): sequential conversions
number `PO-000001..N`, a failed conversion burns no number (the next success
is still sequential), concurrent conversions of different requisitions form
one consecutive run, and the two tenants' counters are independent. The seed
cannot prove this: it writes POs straight into `demo.purchase_orders`
(`scripts/seed`), which never passes through the conversion path.

**Events.** One `po.created` outbox row per conversion (entity =
`purchase_order`/the PO id, `actor_id` = caller, `trace_id` = propagated REST
trace, version 1). Two segments so the §14.5 `po.*` bindings deliver it; it
**extends the §14.6 catalog** (whose first PO event is `po.issued`, #36's)
the same way #31's `pr.step_approved` did. Pinned payload:
`{number, requisition_id, requisition_number, vendor_id, total_cents,
currency, created_by}` — `requisition_number` so a PO timeline can link back
to its requisition (#43). **Known gap (conscious)**: closing the requisition
writes **no requisition-side event** — submit/cancel also write none today;
only Decide writes `pr.*` rows. The requisition timeline (#43, already
shipped) therefore does not show the conversion, despite FR-11's "every
state change appears in document timeline". A `pr.converted`/`pr.closed`
event is a follow-up candidate for #36/#45.

Error codes: `po_vendor_id_invalid` (empty/non-UUID vendor_id, rejected
before the transaction opens), `po_currency_invalid`, `po_items_empty` → 400
· `po_requisition_not_approved` → 409 · `requisition_not_found`,
`vendor_not_found` → 404 (both also the cross-tenant answer) ·
`requisition_id_invalid` → 400 · `vendor_deactivated` → 412. The gateway
carries them through the shared gRPC→HTTP mapping (`Aborted` → 409,
`FailedPrecondition` → 412).

## PO lifecycle state machine (#36)

Ten legal edges over the seven 0007 statuses (FR-06): the linear happy path
`draft → issued → acknowledged → partially_received → received → closed`, a
loose cancel (`draft`, `issued`, and `acknowledged` may all cancel, subject
to the receipt guard), and a short-close (`partially_received → closed` —
accepting the shortfall). `closed` and `cancelled` are terminal. Every other
move — self-loops, skips, and anything out of a terminal status — is
`po_illegal_transition` (HTTP 409) with **no** state change and **no** event.

```
draft ──issue──▶ issued ──acknowledge──▶ acknowledged ──receipt──▶ partially_received ──receipt──▶ received
  │                 │                        │                        │                               │
  └──── cancel ─────┴──────── cancel ────────┘                        └───────── short-close ──────────┤
                                                                                (receipts pin the PO:         ▼
                                                                                 no cancel past this point)  closed
```

- **Cancel guard (receipts)** — cancel is legal only from
  draft/issued/acknowledged **and** while the total `qty_received` over all
  lines is 0. Any receipt pins the PO: `po_has_receipts` (409), nothing
  written; the only exit from partially_received/received is `close`. The
  edge check wins over the receipt check — cancelling a received PO answers
  `po_illegal_transition`, not `po_has_receipts`.
- **Receipt edges are domain-owned since #36 but driven by #37** — goods
  receipts advance `po_items.qty_received` and call
  `MarkPartiallyReceived`/`MarkReceived` under the same row lock (the #28
  `Close` precedent: the method exists and is tested before its driver).
- **Issue stamps `issued_at`** (NULL while draft, set on draft → issued,
  preserved afterwards) and starts the acknowledgement SLA clock (PRD §16:
  reminders for POs awaiting acknowledgement > 3 days).

**Store contract** (`purchaseorder.Store.Issue/Acknowledge/Cancel/Close`):
one tenant transaction per transition with a pinned step order — load the PO
(header + items) `FOR UPDATE` on the row lock (the serialization point every
lifecycle transition shares; #37's receipts will take the same lock, so a
concurrent issue/receipt pair yields exactly one winner), run the domain
method (guards + one aggregate event), land a guarded
`UPDATE … WHERE id = $1 AND status = '<from>'` (the from-status re-check is
belt and braces — losing that race is `po_illegal_transition`), and append
**exactly one** outbox row. An illegal transition or the receipt guard fails
before the UPDATE, so the whole transaction rolls back: no status move, no
outbox row.

**Events.** One outbox row per legal transition (entity =
`purchase_order`/the PO id, `actor_id` = caller, `trace_id` = propagated REST
trace, version 1): `po.issued` and `po.acknowledged` are §14.6 catalog
entries; `po.cancelled` and `po.closed` **extend the catalog** (two segments
so the §14.5 `po.*` bindings deliver them — the same documented extension as
#35's `po.created` and #31's `pr.step_approved`). Pinned payload shared by
all four: `{number, requisition_id, vendor_id, status, total_cents,
currency, performed_by}` (`status` = the new status; `performed_by` = the
actor, self-contained the way `DecisionPayload.decided_by` is). The receipt
transitions write no outbox rows until #37 defines their event shape. The
aggregate also keeps an in-memory event log (`Events()`/`PullEvents()`, the
requisition pattern) with aggregate-local names (`po_issued`, …) — creation
still records none in memory (dev-note 035: `po.created` is the outbox row).

**API** (all `po:write` — buyer/admin at gateway and interceptor): gRPC
`IssuePurchaseOrder` / `AcknowledgePurchaseOrder` / `CancelPurchaseOrder` /
`ClosePurchaseOrder`; REST `POST /api/v1/purchase-orders/{id}/issue` (PRD
§15) plus `/acknowledge`, `/cancel`, `/close` — no request body, the path id
names the PO, the response is **200 + the updated PO**. Error codes:
`po_id_invalid` → 400 · `po_not_found` → 404 (also the cross-tenant answer,
via RLS) · `po_illegal_transition`, `po_has_receipts` → 409 (FR-06's
"invalid transitions rejected with 409", through `Aborted` in the shared
gRPC→HTTP mapping).

## Requisition state machine (#28)

Seven legal edges (loose cancel — maintainer decision on #28):
`draft → submitted`, `draft → cancelled`, `submitted → approved`,
`submitted → rejected`, `submitted → cancelled`, `approved → cancelled`,
`approved → closed`. `rejected`, `cancelled`, `closed` are terminal.
`approved → closed` is driven by PO conversion (#35); the domain `Close`
method exists now. Every successful transition appends exactly one domain
event (`requisition_submitted`, …, plus `requisition_created` on draft
creation) to the in-memory aggregate log (`Events()` / `PullEvents()`).
Since #31 the decide events and since #45 the `pr.submitted` event are
persisted to `procurement.outbox` in the same transaction as their state
change; the create/draft event deliberately stays in-memory only — the
§14.6 catalog names no create event (`pr.submitted` is the first published
step of the lifecycle) — until a catalog decision says otherwise.

Error codes: `requisition_currency_invalid`, `requisition_description_invalid`,
`requisition_qty_invalid`, `requisition_unit_price_invalid`,
`requisition_items_empty`, `requisition_id_invalid`,
`requisition_idempotency_key_invalid`,
`requisition_status_invalid`, `requisition_page_invalid` → 400 ·
`requisition_illegal_transition` → 409
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
- **List** (#32) — `GET /api/v1/requisitions?q=&status=&page=&page_size=`
  (`requisition:read` — every role): one page of **header-only** rows
  (`items`/`approval_steps` always empty; the detail reads own the bodies)
  ordered by `number` **descending** (numbers are sequential per tenant, so
  newest first) plus `{total, page, page_size}`. `q` matches a number prefix
  (case-insensitive; LIKE wildcards literal); `status` is one of the six
  lifecycle values or empty; pagination defaults 1/20, max size 100, absurd
  pages → 400 `requisition_page_invalid`, unknown status → 400
  `requisition_status_invalid`. See the requisition list section below for
  the query shape, budget test, and plans.

## Error codes

`vendor_code_invalid`, `vendor_name_invalid`, `vendor_email_invalid` → 400 · `vendor_duplicate_code` → 409 · `vendor_not_found` → 404 · `vendor_deactivated` → 412. The gateway maps gRPC codes to HTTP and carries the machine code through in the JSON body (`{"error": "<human>", "code": "<machine>"}`) — the same shared mapping serves vendors and requisitions (`InvalidArgument`→400, `NotFound`→404, `AlreadyExists`/`Aborted`→409, `FailedPrecondition`→412, `PermissionDenied`→403).

## Planned follow-ups

- #26: attachments via presigned uploads (done).
- #27: list/search/filter/pagination (done).
- #28: requisition domain + state machine + tables (done).
- #29: requisition create/edit/submit API (done — store, number generation, idempotent submit, REST/gRPC wiring, gateway `Aborted` → 409). Read endpoints: **list done in #32** (see below); get/timeline are separate issues.
- #30: approval rules + sequential engine + decide API (done — matrix materialization, 412 rollback, self/role enforcement, reject-skips, admin rule configuration, steps in-band).
- #31: idempotent decide + outbox audit/notification events (done — `procurement.outbox` + `decision_idempotency` in 0006, decide replay/conflict contract, `pr.*` decide events with actor + trace, gateway `x-trace-id` propagation). **Re-sequenced with #45** by an agent decision: the outbox table + writer moved from #45's scope into #31 (see dev-note 031's provenance block); #45 keeps the publisher loop only.
- #45: transactional outbox publisher (done — worker publisher loop: poll 2 s / batch 50, §14.5 envelope to the durable topic exchange `procurement.events` with confirm-mode persistent publishes, `published_at` marking, non-fatal poll/publish errors, `pr.submitted` written by the Submit transaction; the PROVISIONAL DLX bootstrap is superseded by #46's full topology). AMQP consumers, retry queues, and DLQ handling remain with the consumer issues; observability (spans, AMQP trace headers, outbox lag metric) is #54. (M2–M3)
- #46: RabbitMQ topology with DLX and TTL retry queues (done — durable `procurement.events`/`procurement.dlx` exchanges, `documents.history`/`notifications`/`exports` queues with DLX arguments, per-queue retry ladders 5 s → 30 s → 5 min returning to their own queue, `procurement.dlq` bound `#`; consumers arrive with #47). (M2–M3)
- #35: PO creation from an approved requisition (done — 0007 tables, draft PO with copied lines, per-tenant gap-free `PO-%06d` numbering, conversion replay contract, tx-scoped active-vendor gate `ForUseTx`, guarded `approved → closed`, one `po.created` outbox event). Goods receipts (#37) and the missing `pr.converted` requisition-side event are follow-ups.
- #36: PO lifecycle state machine (done — ten legal edges over the seven statuses, receipt-guarded cancel, `issued_at` on issue, guarded tenant-scoped transitions serialized on the PO row lock, one outbox event per transition, gRPC + REST issue/acknowledge/cancel/close at `po:write`). Goods receipts (#37) drive the receipt edges.
- Approvals read endpoints/queue #32-34 (M2–M3).
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

## Requisition list — filters, pagination, query plans (#32)

`ListRequisitions` (`requisition:read`, REST
`GET /api/v1/requisitions?q=&status=&page=&page_size=`) returns one page of
header-only rows ordered by `number DESC` (newest first — numbers are
sequential per tenant) plus the total number of matching rows:

- `q` — requisition **number prefix**, case-insensitive. LIKE wildcards in
  the input (`%`, `_`, `\`) are escaped, so they match literally. Numbers are
  canonical server-generated ASCII (`PR-%06d`), which is why two
  requisition-specific plan choices below are safe.
- `status` — one of the six lifecycle values or empty (all); combines with
  `q`.
- `page` (1-based, default 1) / `page_size` (default 20, max 100). Responses
  echo the normalized values; `total` counts matches across all pages. A
  page past the end is empty but still reports the true `total` (an explicit
  COUNT fallback).

The total rides the same single statement as the page: an exact COUNT as an
uncorrelated scalar subquery (an executor **InitPlan — evaluated exactly
once**), attached to the `LIMIT/OFFSET` page subquery. This deliberately
differs from the vendor list's `count(*) OVER()` (#27), for measured reasons
at 100k rows / default `work_mem` (4 MB) on the dev machine:

1. **`count(*) OVER()` spills.** The window's tuplestore materializes every
   matching row (~12 MB at 100k) and went to temp files: 217–540 ms per
   unfiltered query against the 300 ms budget.
2. **A `CROSS JOIN (SELECT count(*) …)` arm is not planner-safe**: without a
   Materialize node the aggregate was re-executed once per page row
   (`loops=20`), turning a 50 ms query into ~1 s. The scalar-subquery form
   is guaranteed single-shot by the executor.

The page arm sorts with `number COLLATE "C" DESC`: every storable number is
canonical uppercase ASCII (`PR-` + digits), so C ordering equals the default
collation's ordering for this data while comparisons stay memcmp-cheap — on
the Windows/libc dev collation (English_Indonesia.1252) the top-N sort alone
dropped from ~140 ms to ~35 ms. The existing
`requisitions_tenant_number_uidx (tenant_id, number)` serves prefix quals in
principle; the #27 note about the RLS policy's bypass arm keeping
tenant-leading index quals unavailable applies unchanged (the planner
estimates the tenant filter at ~1% and prefers scans).

### Measured plans at 100k rows (the #32 budget test)

`TestStoreListScale100k` (services/procurement/requisition) seeds 100,000
requisitions in one tenant (80% draft / 10% submitted / 10% cancelled),
brings the table to its production steady state (`VACUUM (ANALYZE)` — what
autovacuum does after a bulk load, setting the visibility map — plus
`CHECKPOINT` so the fixture's dirty buffers don't compete with the timing
loop), then runs every filter shape 30× under the real RLS context after
warm-ups. Budget: **p95 < 300 ms** per query, same bar as #27. Measured on
the dev machine:

| Shape | median | p95 |
|---|---|---|
| unfiltered page 1 | ~64–88 ms | ~108–134 ms |
| status = submitted (10% of rows) | ~8–10 ms | ~15–18 ms |
| number prefix (~1k rows) | ~56–156 ms | ~168–251 ms |
| status + number prefix combined | ~74–161 ms | ~179–258 ms |

Steady-state plans (from the test log's EXPLAIN (ANALYZE)):

- **Unfiltered**: the count arm runs as `InitPlan 1 → Aggregate → Index Only
  Scan using requisitions_tenant_idx` (100k index entries, `Heap Fetches: 0`
  thanks to the vacuumed visibility map, ~27 ms); the page arm is a top-N
  heapsort over the scan with the `COLLATE "C"` key (~83 ms); total ~110 ms.
- **status filter**: `Index Only Scan using requisitions_tenant_status_idx`
  with `Index Cond: (status = 'submitted')` and the RLS policy as a filter —
  the existing `(tenant_id, status)` index carries the selectivity; ~10 ms.
- **number prefix / combined**: parallel seq scans evaluating the LIKE
  prefix per row; the LIKE (not the sort) dominates (~50–160 ms on the dev
  collation; far less on Linux/glibc).

The prefix sort cost is also why the RLS policy's per-row
`tenant_id::text = current_setting(…)` evaluation matters at this scale —
same #27 trade-off; a platform-level policy reshaping remains a follow-up.
