# Documents service — attachments and event history

Two storages in one service: attachment metadata (MongoDB `document_meta`)
plus the MinIO object lifecycle, and the append-only event history (MongoDB
`events` + `audit_log`, PRD §9/§13/§14.3) with the `ListEvents` read API.
Attachment bytes never stream through the gateway or this service: clients
upload to and download from presigned URLs (15-minute validity, SigV4).

## Lifecycle

```text
declare → pending metadata + presigned PUT
   │  client PUTs bytes directly to MinIO (Content-Type: declared mime)
   ▼
complete → verify (size == declared, content type allowed, sha256 matches)
   │  ok → ready (uploaded_at set; presigned GET available)
   │  mismatch → object deleted, metadata stays pending (client may re-upload)
   ▼
reaper → pending older than ATTACHMENT_PENDING_TTL (default 24h):
         object removed best-effort, metadata dropped
```

- Object keys follow PRD §13: `attachments/{tenant_id}/{entity}/{uuid}`.
  Entity types and ids are validated to a path-safe charset before they
  become key segments; accepted owner kinds are `vendor` (#26),
  `requisition` (#33), and `invoice` (#39).
- Declarations are bounded: mime allowlist (pdf, png, jpeg, webp, docx,
  xlsx), `ATTACHMENT_MAX_SIZE_MB` (default 25), filename 1–255 chars with no
  path separators, checksum must be sha256 hex (lowercased on accept).
- `CompleteUpload` is idempotent (re-completing a ready attachment is a
  no-op). Verification failures keep the metadata pending so a client can
  upload correct bytes and complete again.
- Tenant scoping is by `tenant_id` in every query — cross-tenant reads are
  indistinguishable from missing (`attachment_not_found`). Only the reaper
  query sweeps across tenants.

## gRPC API (`documents.v1`, port 8083)

| Method | Permission | Notes |
|---|---|---|
| `CreateUpload` | per entity: `vendor:write` / `requisition:write` / `invoice:write` | validates the declaration, returns presigned PUT + pending metadata |
| `CompleteUpload` | per entity: `vendor:write` / `requisition:write` / `invoice:write` | verifies size/mime/checksum, flips to `ready` |
| `ListAttachments` | per entity: `vendor:read` / `requisition:read` / `invoice:read` | by owner (entity type + id), oldest first |
| `GetDownloadUrl` | per entity: `vendor:read` / `requisition:read` / `invoice:read` | ready attachments only, returns presigned GET |
| `ListEvents` | `requisition:read` | interceptor map (single permission) |

Since #33 the attachment methods are **absent from the interceptor's
methodPermissions map on purpose**: their permission depends on the
attachment's entity type, which the interceptor cannot see before the call
runs. Each handler resolves the entity type (from the request for
CreateUpload/ListAttachments, from the stored attachment for
CompleteUpload/GetDownloadUrl) and re-checks the per-entity permission
in-handler via `rbac.Check` — the same defense-in-depth depth, one level
deeper. Unknown entity types fail with 400 `attachment_entity_invalid`,
never a silent pass.

## REST surface (gateway)

| Route | Permission | Body / result |
|---|---|---|
| `POST /api/v1/vendors/:id/attachments` | `vendor:write` | `{filename, mime_type, size_bytes, checksum_sha256}` → `201` with `upload: {url, method, expires_at, headers}` |
| `POST .../attachments/:attachmentId/complete` | `vendor:write` | → attachment metadata, `status: ready` |
| `GET .../attachments/:attachmentId/download` | `vendor:read` | → `download: {url, expires_at}` |
| `GET /api/v1/vendors/:id` | `vendor:read` | vendor detail embeds `attachments: [...]` |
| `POST /api/v1/requisitions/:id/attachments` (#33) | `requisition:write` + requester ownership | same body → `201` with the presigned PUT envelope |
| `POST /api/v1/requisitions/:id/attachments/:attachmentId/complete` (#33) | `requisition:write` | → attachment metadata, `status: ready` |
| `GET /api/v1/requisitions/:id/attachments/:attachmentId/download` (#33) | `requisition:read` | → `download: {url, expires_at}` |
| `GET /api/v1/requisitions/:id` (#33) | `requisition:read` | requisition detail embeds `attachments: [...]` (absent when documents is not wired) |
| `POST /api/v1/invoices/:id/attachments` (#39) | `invoice:write` | same body → `201` with the presigned PUT envelope |
| `POST /api/v1/invoices/:id/attachments/:attachmentId/complete` (#39) | `invoice:write` | → attachment metadata, `status: ready` |
| `GET /api/v1/invoices/:id/attachments/:attachmentId/download` (#39) | `invoice:read` | → `download: {url, expires_at}` |
| `GET /api/v1/invoices/:id` (#39) | `invoice:read` | invoice detail embeds `attachments: [...]` (absent when documents is not wired) |

The gateway verifies the owner exists (procurement `GetVendor` /
`GetRequisition` / `GetInvoice`) before any documents call. The requisition declare route
additionally mirrors procurement's ownership rule — the caller must be the
draft's `requester_id` (admins excepted), else 403 `requisition_forbidden`;
attachments on a requisition are writes to it. Complete/download only bind
the attachment to the path's owner id (vendor parity). `object_key` never
appears in REST responses. Machine error codes (`attachment_too_large`,
`attachment_checksum_mismatch`, …) pass through unchanged; validation →
400, missing → 404, lifecycle violations → 412. The human message for
`attachment_entity_invalid` is neutral ("attachment owner type is not
supported") because the mapping is shared by the vendor, requisition, and
invoice routes. These requisition routes are sanctioned by the #33 issue
decision (agent 2026-10-25); PRD §15 does not list them.

## Storage

- **MongoDB** `document_meta` (`MONGO_URI` / `MONGO_DB`): indexes
  `owner_lookup` `{tenant_id, entity_type, entity_id, created_at}` and
  `reaper_pending` `{status, created_at}` (created on boot).
- **MinIO** (`S3_ENDPOINT` host:port, no scheme; `S3_USE_SSL` for TLS):
  bucket `attachments` (default, created on boot). Credentials:
  `S3_ACCESS_KEY` / `S3_SECRET_KEY` (= `MINIO_ROOT_*` locally).
- **Reaper job**: runs inside the documents service every
  `REAPER_INTERVAL` (default 30m; `0` disables). It is a singleton there —
  when the worker service takes over cron with Redis locks, the sweep moves
  with it.

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `MONGO_URI` | `mongodb://eprocurement:eprocurement@localhost:27017/?authSource=admin` | MongoDB connection |
| `MONGO_DB` | `eprocurement` | database name |
| `S3_ENDPOINT` | `localhost:9000` | MinIO host:port, no scheme |
| `S3_PRESIGN_ENDPOINT` | *(empty = `S3_ENDPOINT`)* | host minted into presigned URLs when clients reach MinIO by a different address (compose: services dial `minio:9000`, clients outside use `localhost:9000`) |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | `eprocurement` | object-store credentials |
| `S3_BUCKET` | `attachments` | bucket (created when missing) |
| `S3_USE_SSL` | `false` | TLS to the object store |
| `ATTACHMENT_MAX_SIZE_MB` | `25` | declaration size limit |
| `ATTACHMENT_PENDING_TTL` | `24h` | reaper threshold for pending uploads |
| `REAPER_INTERVAL` | `30m` | sweep cadence; `0` disables |

Integration tests run against real MongoDB + MinIO and are gated on
`TEST_MONGO_URI` + `TEST_S3_ENDPOINT` (set for local runs and in CI;
`go test ./...` skips them otherwise).

## Event history and audit log (#42)

`services/documents/history` is the append-only projection of procurement
domain events (PRD §14.3 CQRS-lite; ADR-0001: no dual writes). `Store.Apply`
is the projection function the worker consumers (#45–#47) will call; the
transport (RabbitMQ) is theirs. Both collections are **append-only** — no
update or delete path exists anywhere in the codebase, and the package
exports only `EnsureIndexes`, the document constructors, `Apply`, and
`List` (PRD §17).

### Collections

`events` — one document per domain event, the PRD §14.5 envelope:

| Field | Type | Notes |
|---|---|---|
| `event_id` | string | unique; the consumer-dedupe key (PRD §14.5) |
| `tenant_id` | string | tenant scoping |
| `type` | string | catalog name, e.g. `pr.approved` — **`type`, not `event_type`**, matching the envelope and the seed |
| `version` | int | payload schema version, 1 today |
| `occurred_at` | date | business time (ms precision) |
| `actor_id` | string | the acting user; empty when unknown |
| `trace_id` | string | propagated trace; empty when unknown |
| `entity_type` | string | aggregate kind, e.g. `requisition` |
| `entity_id` | string | aggregate id |
| `payload` | string | **raw JSON string** — full fidelity of the procurement outbox jsonb (no key reordering, no number re-encoding); `ListEvents` returns it verbatim as `payload_json` |

`audit_log` — one document per **decision** event (`pr.approved`,
`pr.step_approved`, `pr.rejected`, `pr.step_rejected` — the decide types of
dev-note 031; `history.AuditAction` maps them to `approve`/`reject`):

| Field | Type | Notes |
|---|---|---|
| `event_id` | string | unique; dedupes audit writes per event |
| `tenant_id` | string | tenant scoping |
| `actor_id` | string | the decider |
| `action` | string | `approve` \| `reject` |
| `entity_type` / `entity_id` | string | the decided aggregate (the requisition) |
| `details` | string | raw JSON payload of the event, verbatim |
| `trace_id` | string | propagated trace |
| `occurred_at` | date | business time |
| `recorded_at` | date | projection time (when Apply wrote the row) |

Decoding tolerates absent fields (zero values, no error) — rows written
before a field existed (e.g. the pre-#42 seed shape without
`version`/`actor_id`/`trace_id`) stay readable. Pinned by
`TestApplyToleratesMissingFields`.

### Indexes

One definition, shared by the service's `EnsureIndexes` and `scripts/seed`
(`history.EventIndexes` / `history.AuditIndexes`), so the two can never
drift. The seed's old auto-named index definitions are gone; reseeding
drops both collections and recreates exactly this set — **before** the bulk
inserts, not after: mongod 7.0.45 intermittently aborts with a WiredTiger
invariant when an index build scans the million-event collection right
after the reset's collection drops, while builds on empty collections
followed by indexed inserts are stable and equally fast (dev-note 042).

| Collection | Index | Keys | Notes |
|---|---|---|---|
| `events` | `uniq_event_id` | `{event_id: 1}` | **unique** — the dedupe mechanism |
| `events` | `tenant_occurred` | `{tenant_id: 1, occurred_at: -1, event_id: -1}` | newest-first pages |
| `events` | `tenant_entity_occurred` | `{tenant_id: 1, entity_type: 1, entity_id: 1, occurred_at: -1, event_id: -1}` | timeline filter (dev-note 031 follow-up) |
| `audit_log` | `uniq_event_id` | `{event_id: 1}` | **unique** — audit dedupe per event |
| `audit_log` | `tenant_occurred` | `{tenant_id: 1, occurred_at: -1}` | newest-first audit reads |

### Apply: idempotent projection

`Apply(ctx, ev)` validates the envelope, inserts the events document, then
— for decision types — the audit_log row. A duplicate-key error on either
insert means "already applied" and is **success, not an error**; the return
value reports whether this call inserted the event (false on replay). Two
inserts are deliberately **not** wrapped in a transaction: the local
MongoDB is a single-node mongod (no replica set, no multi-document
transactions). Each insert is individually idempotent on `event_id`, so a
crash between the two is recovered by the next Apply / consumer retry —
the event insert dedupes, the audit insert lands
(`TestApplyPartialRecovery`).

### ListEvents

`GET`-style read of the caller tenant's history (the gateway route lands
with #43). Contract:

- order is total and stable: `occurred_at DESC`, `event_id DESC as the
  tiebreaker` — decide events share `occurred_at` (step + terminal pair,
  approval/store.go) and still order deterministically;
- pagination is **keyset** (a deliberate deviation from the repo's
  page/page_size convention — see dev-note 042): `next_cursor` is the
  opaque position of the page's last event; pages never duplicate or skip
  rows regardless of concurrent inserts; empty on the last page;
- `entity_type` + `entity_id` filter one aggregate's timeline — both set
  or both empty, else `InvalidArgument` `events_filter_invalid`;
- `limit` default 20, max 100, clamped like the vendor list; a malformed
  cursor → `InvalidArgument` `events_cursor_invalid`;
- `payload_json` returns the stored payload string verbatim;
- tenant scoping is absolute: another tenant's events are invisible
  (empty result), pinned at store and gRPC level.

### gRPC API addition (documents.v1)

| Method | Permission | Notes |
|---|---|---|
| `ListEvents` | `requisition:read` | tenant's event history, newest first, cursor-paginated |

`requisition:read` is held by every tenant role (rbac matrix), so the whole
tenant can read its own timeline; the mapping is mandatory because methods
missing from `MethodPermissions` pass through unprotected (rbac/grpc.go).
No new permission was added — `internal/platform/rbac` and `docs/rbac.md`
are untouched.

## Notifications projection (#47)

`services/documents/notifications` is the second projection the worker
consumes RabbitMQ for (PRD §14.5 queue `notifications`): exactly **one
document per event** in the MongoDB `notifications` collection, written by
the worker's notifications consumer. The queue bindings — the actor-facing
subset of the §14.6 catalog (`pr.submitted`, `pr.approved`, `pr.rejected`,
`po.issued`, `po.acknowledged`, `invoice.discrepancy`) — live with the
worker's consumer topology; the package itself is only the projection
(`EnsureIndexes` + `Apply`), the same discipline as history.

### Collection

`notifications` — the PRD §14.5 envelope plus the derived recipient:

| Field | Type | Notes |
|---|---|---|
| `event_id` | string | unique; the consumer-dedupe key (PRD §14.5) |
| `tenant_id` | string | tenant scoping |
| `type` | string | catalog name, e.g. `pr.approved` |
| `version` | int | payload schema version, 1 today |
| `occurred_at` | date | business time |
| `actor_id` | string | the acting user; empty when unknown |
| `trace_id` | string | propagated trace; empty when unknown |
| `entity_type` / `entity_id` | string | the aggregate the event is about |
| `recipient_id` | string | **derived**: `payload.requester_id`, fallback `payload.submitted_by`; may be empty — the document is still created (the effect is durable per event either way) |
| `payload` | string | raw JSON payload of the event, verbatim |
| `recorded_at` | date | projection time (when Apply wrote the document) |

### Indexes (PRD §14.7)

| Index | Keys | Notes |
|---|---|---|
| `uniq_event_id` | `{event_id: 1}` | **unique** — the dedupe mechanism; created by the worker's boot `EnsureIndexes` before consumption starts |
| `tenant_occurred` | `{tenant_id: 1, occurred_at: -1}` | compound tenancy index; a future read side pages a tenant's notifications newest-first |

### Apply

`Apply(ctx, ev)` validates the envelope through the shared `history.Event`
contract (the validation sentinel errors are history's — the consumer
classifies them as permanent and dead-letters without retry), derives the
recipient from the payload, and inserts one document. A duplicate-key error
on `uniq_event_id` is a **successful replay** (`applied=false`), so a
redelivered event never creates a second notification. There is no update
or delete path — the collection is append-only (PRD §17).
