# 042 — Documents service with MongoDB event store

- **Issue:** #42
- **Branch:** feature/42-documents-mongo-event-store
- **Date:** 2026-10-24

## What

- **`services/documents/history` (new)** — the append-only event store
  (PRD §9/§13/§14.3, ADR-0001): MongoDB collections `events` and
  `audit_log`, both grown only through `Store.Apply` (no update/delete path
  exists; the package exports only `EnsureIndexes`, the document
  constructors, `Apply`, and `List` — PRD §17).
  - `Apply(ctx, ev) (applied bool, err)`: validates the §14.5 envelope
    (typed machine-code errors), inserts the events document, then — for
    decision types — the audit_log row. Duplicate-key on either unique
    `event_id` index is a **successful replay** (applied=false), not an
    error. The two inserts are deliberately **not** in a transaction: the
    local MongoDB is a single-node mongod (no replSet), where multi-document
    transactions do not exist. Each insert is individually idempotent on
    `event_id`, so a crash between them is completed by the next Apply /
    consumer retry.
  - `AuditAction` maps the four decide event types of dev-note 031
    (`pr.approved`, `pr.step_approved`, `pr.rejected`, `pr.step_rejected`)
    to audit actions `approve`/`reject`; the strings are repeated here
    because documents must not import the producer's domain.
  - `List(ctx, tenant, filter)`: tenant-scoped, `occurred_at DESC,
    event_id DESC` (total order — decide events share occurred_at,
    approval/store.go), keyset cursor pagination, optional
    entity_type+entity_id timeline filter (both-or-neither).
  - Pinned indexes, one definition for service and seed
    (`EventIndexes`/`AuditIndexes`): `uniq_event_id {event_id:1}` unique,
    `tenant_occurred {tenant_id:1, occurred_at:-1, event_id:-1}`,
    `tenant_entity_occurred {tenant_id:1, entity_type:1, entity_id:1,
    occurred_at:-1, event_id:-1}` on `events`; `uniq_event_id` unique +
    `tenant_occurred {tenant_id:1, occurred_at:-1}` on `audit_log`. This
    lands the tenant+entity index dev-note 031 deferred to the first reader.
- **Proto `documents.v1`** — additive `ListEvents` rpc + `Event` /
  `ListEventsRequest` / `ListEventsResponse` messages; `gen/go` regenerated
  (`make gen-proto`), only documents stubs changed.
- **documents server** — `"/documents.v1.Documents/ListEvents"` mapped to
  `rbac.RequisitionRead` in `methodPermissions`; `api.ListEvents` reads the
  caller tenant from the auth metadata, delegates to the store, and maps
  typed store errors to `InvalidArgument` with the machine code
  (`events_filter_invalid`, `events_cursor_invalid`, …). `cmd/documents`
  boots the history store and its indexes alongside the attachment store.
- **`scripts/seed`** — events now built through `history.NewEvent` /
  `EventBSON` / `NewAuditEntry` / `AuditBSON` (anti-drift: the seed writes
  exactly what Apply writes) and enriched with `version=1`, `actor_id`
  (the requisition's deterministic requester), `trace_id` (deterministic
  32-hex from the seeded RNG), so ListEvents over seed data shows full
  rows. Decision types (`pr.approved`/`pr.rejected` in the seed's type
  universe) also write their audit_log rows (~200k at the default 1M
  events; `recorded_at` = `occurred_at` — the seed backfills history as if
  recorded at occurrence). `resetData` now drops **both** `events` and
  `audit_log`, so repeated deterministic runs can never hit E11000 and the
  old auto-named index set cannot survive a reseed.
- **Docs** — `docs/schema/documents.md` extended with the collections,
  document shapes, index set, Apply/ListEvents contracts, and permission.

## Decisions

- **Keyset cursor pagination, deviating from the repo's page/page_size
  convention** (`procurement.proto` ListVendors/ListRequisitions use
  page/page_size/total). A timeline is unbounded and read-while-writing:
  offset pagination skips or duplicates rows when new events land between
  pages (the worker projects continuously), and `total` is meaningless on
  an append-only stream. The cursor is the page's last
  `(occurred_at ms, event_id)` base64-encoded; continuation is a strict
  `$or` keyset on the same index that serves the sort, so pages are stable
  under concurrent inserts. The gateway route (#43) inherits this shape —
  flagged there. Limit normalizes like the vendor list (below 1 → 20,
  above 100 → 100) rather than erroring, matching `vendor.ValidateList`;
  only the cursor and the half entity filter return `InvalidArgument`.
- **`ListEvents` maps to `requisition:read`, not a new `events:read` or
  `audit:read`.** The events' entities today are requisitions and the
  timeline is the requisition detail's timeline (#43); `requisition:read`
  is granted to every tenant role (rbac matrix), which matches PRD §10's
  "audit/event history viewer per document" for the whole tenant.
  `audit:read` is viewer-only — wrong audience for a requisition timeline.
  Critically, no new permission was added (docs/rbac.md and
  `internal/platform/rbac` are untouched): methods absent from
  `MethodPermissions` pass through **unprotected** (rbac/grpc.go), so
  mapping onto an existing permission is also the smallest safe diff. The
  denial test uses a role outside the matrix (every seeded role holds
  requisition:read; `rbac.Check` grants unknown roles nothing).
- **`payload` stored and returned as a raw JSON string** (`payload_json`
  in the API), not a protobuf `Struct` and not a BSON subdocument: full
  fidelity of the procurement outbox jsonb (no key reordering, no number
  re-encoding), no well-known-type imports, and the API needs no schema
  knowledge of payloads. Trade-off: Mongo aggregations cannot reach into
  `$payload` fields — no current reader does; revisit if an aggregation
  pipeline ever needs payload fields.
- **Seed index creation moved before the bulk insert** (was: after). The
  planned order — bulk insert 1M events, then build indexes — intermittently
  crashed mongod 7.0.45 with a WiredTiger invariant
  (`__wt_txn_context_prepare_check` → `invariant cursor->get_key` in
  `wiredtiger_record_store.cpp`, `MultiIndexBlock` collection scan; server
  exit 6/14, container down) right after the reset's collection drops.
  Reproduced with `CreateMany` and with per-index `createIndex` alike;
  per-index builds on a 1M-doc scratch collection succeeded, so the trigger
  is the big scan after ident drops, not the command shape. Creating the
  index set on the freshly-dropped (empty) collections and then inserting
  is stable — four consecutive full seeds, identical timings (~34 s vs
  ~34 s) and counts. `EnsureIndexes` itself is unchanged `CreateMany`:
  against an indexed DB it is a no-op and against an empty one it scans
  nothing; the service boots cleanly against the seeded 1M-event DB
  (verified live). Follow-up below pins the patch-level question.
- **Document shapes pinned by test, not just docs**: `TestEventBSONShape`
  / `TestAuditBSONShape` (field names incl. `type`, not `event_type`) run
  without Mongo; `TestApplyToleratesMissingFields` pins that rows missing
  `version`/`actor_id`/`trace_id`/`payload` (the pre-#42 seed shape) decode
  to zero values instead of erroring.
- **Partial-recovery test simulates the failed audit write** by deleting
  the audit row after a successful Apply (a real mid-call Mongo failure
  cannot be injected without a mock store; the delete produces exactly the
  crashed-between-inserts state) and then re-applying.
- **One-time reseed needed on dev volumes**: existing volumes carry the
  old auto-named indexes (`event_id_1`, `tenant_id_1_occurred_at_-1`) from
  the pre-#42 seed. One `make seed` run drops both collections (indexes
  included) and recreates only the named set — verified via
  `db.events.getIndexes()`.

## Evidence

All local runs against the compose stack (`docker compose up -d mongo
minio postgres`; Mongo 7.0.45 single-node, PostgreSQL 16, MinIO).

- `make gen-proto` — regenerated; only `gen/go/documents/v1/*.pb.go`
  changed (procurement stubs showed line-ending noise only, no content
  diff).
- `go vet ./... && go build ./...` — clean.
- `TEST_MONGO_URI=… go test ./services/documents/history/ -count=1` —
  green, 17 tests: BSON shape pins, envelope validation table, audit
  action table, cursor codec round-trip + invalid table, filter
  normalization; integration (real Mongo): `TestApplyIdempotent`
  (decision event applied twice → 1 events doc + 1 audit row, replay
  returns applied=false, non-decision event writes no audit),
  `TestApplyPartialRecovery` (audit row deleted → re-Apply recreates it,
  event not duplicated), `TestListNewestFirstTiebreak` (shared
  occurred_at pair ordered by event_id DESC), `TestListCursorPagination`
  (limit-2 pages over 5 events → exact order, 3 pages, empty final
  cursor), `TestListEntityFilterAndCrossTenant` (tenant B sees only its
  own event; half filter → `ErrFilterInvalid`; bad cursor →
  `ErrCursorInvalid`), `TestApplyToleratesMissingFields`,
  `TestEnsureIndexes` (names + key patterns + directions + unique flags
  exactly the documented set, via `ListSpecifications`).
- `TEST_MONGO_URI=… TEST_S3_ENDPOINT=… go test ./services/documents/...
  -count=1` — all documents packages green, including the new gRPC tests
  in `services/documents/server`: `TestListEventsOrderPaginationTenant`
  (full wiring: 3-page cursor walk, field fidelity incl. payload_json,
  entity filter, cross-tenant denial), `TestListEventsValidation`
  (machine codes `events_filter_invalid` / `events_cursor_invalid`;
  limit 1000 clamps and answers), `TestListEventsRBAC` (unknown role →
  `PermissionDenied`, no metadata → `Unauthenticated`, viewer reads fine).
- `go test ./... -count=1` (no test env) — whole repo green, DB-gated
  suites skip as designed.
- `go run ./scripts/seed --tenants 3 --vendors-per-tenant 20
  --requisitions 2000 --events 20000` — 0.9–1.1 s; `--reset` then
  verified `events: 0 audit_log: 0`; rerun identical counts (no E11000).
- **Full default seed (`make seed`, 1M events), 4 consecutive runs after
  the index-ordering fix: 34.2 s / 34.0 s / 34.1 s / 33.4 s** (budget
  2 min; wall time incl. `go run` compile ~37 s). Counts identical every
  run: `events: 1000000 audit_log: 200851`. `db.events.getIndexes()` /
  `db.audit_log.getIndexes()` show exactly the documented set (+ `_id_`),
  unique flags correct. Before the fix, one run crashed mongod mid-index
  build (see Decisions).
- `make seed-reset` after a full seed — `events: 0 audit_log: 0`.
- Query shapes on the seeded 1M-event DB (mongosh, 50 iterations):
  page-1 43 ms, cursor page 38 ms, entity-filtered page 29 ms —
  ~0.6–0.9 ms/query, all index-backed; newest seeded event carries
  `version: 1`, `actor_id`, `trace_id`, and a decision payload.
- Service boot against the seeded DB (`MONGO_URI=… S3_ENDPOINT=…
  go run ./services/documents/cmd/documents`, killed after 12 s) —
  starts cleanly, reaper up, no index-build scan (EnsureIndexes no-op).
- pnpm build/lint/test not run by me (no TypeScript touched); the
  workflow script re-runs the full gate set after this dev note.
- Compose/seed files: `docker compose config` not re-run (no compose
  changes; only mongo/minio/postgres services were started for tests).

## Follow-ups

- **mongo:7.0.45 index-build crash** (WiredTiger invariant during large
  collection scans after ident drops, observed on Docker Desktop/WSL2):
  the seed now avoids big index builds entirely, but a service boot
  against a large *unindexed* events collection would still hit the scan
  path. Options for the maintainer: pin an earlier 7.0.x patch in
  `compose.yaml` + CI (infrastructure change — deliberately not done
  here), or accept the current ordering guarantee (seeded DBs are always
  indexed before the service boots).
- #43 (gateway route) inherits the cursor contract and needs the same
  deviation note on `GET /api/v1/events`; #44 (audit view) reads
  `audit_log` — a `ListAudit` API is still to come (this issue only
  writes the rows).
- #45–#47 wire the actual consumers: the worker's history consumer calls
  `history.Store.Apply` with the §14.5 envelope; the envelope→`Event`
  translation (payload already JSON on the wire) lands there.
- Submit/create requisition events are still not in the outbox (dev-note
  031 follow-up) — the seed's `pr.created`/`pr.submitted` rows currently
  represent future traffic.
- Aggregation pipelines over payload fields would need payload as BSON
  subdocuments instead of the fidelity-first JSON string — revisit only
  when a reader needs it.
