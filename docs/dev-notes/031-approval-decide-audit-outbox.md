# 031 — Idempotent approve/reject with outbox audit and notification events

- **Issue:** #31
- **Branch:** feature/31-approval-decide-audit-outbox
- **Date:** 2026-10-24

> **Provenance:** the maintainer was unavailable while #31 was prepared, so
> the open questions were decided by the agent on 2026-10-24 ("Decisions
> applied by agent 2026-10-24, maintainer unavailable — override welcome in
> PR review" — issue #31 comment 2; not a maintainer answer). The applied
> decisions, all open to override in PR review and flagged in the PR
> description:
>
> 1. **Outbox table + writer moved into #31** from #45's official scope
>    ("outbox table in procurement; write in same transaction as state" —
>    `gh issue view 45`), because #31's technical note ("Events go through
>    the outbox") needs a persistent destination for decision events. This
>    is deliberate re-sequencing, not scope creep: #45 keeps the publisher
>    loop (RabbitMQ dispatch, retries, DLQ, `published_at`).
> 2. **"Timeline shows decision"** read as decisions recorded as complete
>    events in the outbox — the source for the timeline projection built by
>    #43/#34. The visible timeline read path (endpoint/UI) needs #42/#47
>    and stays out of this PR (one issue per PR, smallest coherent diff).
> 3. **"audit_log entries with actor and trace"** read as outbox rows
>    carrying `actor_id` (= decided_by) and `trace_id`. The Mongo
>    `audit_log` collection remains a documents projection (PRD §14.7),
>    untouched here.

## What

- **Migration `0006_outbox_decide_idempotency.sql`** —
  `procurement.outbox` (id = event_id, tenant_id, event_type, `version`
  int default 1, payload jsonb, actor_id, trace_id, entity_type/entity_id,
  occurred_at, created_at, published_at nullable; partial index
  `(created_at) WHERE published_at IS NULL` for the #45 poller; RLS tenant
  pattern, grants SELECT/INSERT/UPDATE, no DELETE — append-only) and
  `procurement.decision_idempotency` (`(tenant_id, idempotency_key)` PK,
  `step_id` FK → approval_steps ON DELETE CASCADE; RLS tenant pattern).
  Auto-embedded via the existing `//go:embed *.sql`.
- **`services/procurement/outbox`** (new) — `Event` + `AppendTx(ctx, tx,
  …)`: inserts rows inside the caller's transaction (guarded: tenant/type/
  entity required; version defaults to 1; payload marshalled to jsonb).
  The publisher loop stays #45.
- **`services/procurement/approval`** — `DecideInput.IdempotencyKey`;
  new sentinels `ErrIdempotencyKeyInvalid` / `ErrIdempotencyKeyConflict`
  (mapped: InvalidArgument → 400, AlreadyExists → 409). Inside the decide
  transaction **after the parent FOR UPDATE lock**: the replay lookup (same
  key + same step → return current requisition + steps, write nothing;
  same key + another step of the tenant → 409 conflict), then the #30
  guarded writes, then the outbox appends, then the key insert — **success
  path only** (the pinned submit semantics, requisition/store.go:231-242).
  Two concurrent keyed decides serialize into one winner + one idempotent
  replay, never a conflict. A duplicate decide without a key keeps the #30
  `approval_step_not_current` 409. Outbox events: exactly one
  `pr.step_approved`/`pr.step_rejected` per decision, plus
  `pr.approved`/`pr.rejected` only when the flow finished; payload carries
  requester_id, number, step_id/step_no, decision, reason, decided_by,
  decided_at; `actor_id` = decider, `trace_id` = `logger.TraceID(ctx)`
  (set by the TraceInterceptor from `x-trace-id`).
- **proto `procurement.v1`** — additive `string idempotency_key = 4` on
  `DecideApprovalRequest`; `gen/go` regenerated (`make gen-proto`) and
  committed.
- **procurement server** — `DecideApproval` trims the key, >64 chars →
  400 `approval_idempotency_key_invalid` (service-layer defense in depth),
  passes it to the store; `mapApprovalError` gained the two arms.
- **gateway** — `approvals.go` handles the `Idempotency-Key` header with
  the submit convention (trim; blank = no idempotency; >64 → 400 without a
  procurement call; two new messages). `vendors.go forwardContext` now
  appends `x-trace-id` (from the trace middleware's `X-Trace-Id`) to the
  outgoing gRPC metadata after `rbac.OutgoingContext`, so every proxied
  call — not just decide — propagates the REST trace.
- **Docs** — `docs/schema/procurement.md`: migration 0006 section (tables,
  the **conscious column deltas vs PRD §13/§14.5**: version, actor_id,
  trace_id, occurred_at, entity_type/entity_id), the pinned decide replay
  contract, the two-segment event-naming contract + §14.6 catalog
  extension, updated error-code table, follow-ups.

## Decisions

- **Two-segment step event names are pinned** (`pr.step_approved`,
  `pr.step_rejected`): PRD §14.5 binds the topic exchange with `pr.*`, and
  the `*` wildcard matches exactly one word — three-segment step names
  would never be delivered by a natural binding. Both are documented as an
  extension of the §14.6 catalog (`pr.approved`/`pr.rejected` are the
  catalog entries themselves).
- **The outbox entity is the requisition** on all four decide events; the
  step rides in the payload. `entity_type`/`entity_id` exist for
  `GET /api/v1/events?entityType=&entityId=` (§15) and the #43 timeline —
  one filter key per aggregate, no per-step filter queries.
- **A dedicated `decision_idempotency` table** instead of a key column on
  `approval_steps`: the key's uniqueness scope is the tenant, and steps
  deliberately have no tenant_id (0005) — a unique key column there would
  let a cross-tenant 23505 leak which keys exist in other tenants. The
  composite PK keeps key existence tenant-scoped.
- **Replay lookup and key insert both run inside the decide transaction
  after the parent lock**, key recorded on success only — the exact
  semantics submit uses, so concurrent same-key decides cannot conflict
  and failed decisions never burn a key. A 23505 mapping on the composite
  PK is kept as belt-and-braces (unreachable behind the lock).
- **The column deltas are recorded as deltas**, not claimed as
  PRD-conformant: §13 lists six outbox columns, §14.5's envelope has
  `version`/`occurred_at`/`actor_id`/`trace_id` as envelope fields. They
  are columns here so #45 assembles envelopes straight from the row.
- **The #30 in-memory aggregate events are left untouched**; the outbox
  rows are written from the landed outcome (decision + final status) with
  the wire names. The in-memory log is now consumer-less — reconciling it
  (or deleting it in favor of the outbox) is a follow-up.
- **`x-trace-id` is appended in `forwardContext`** (after
  `rbac.OutgoingContext`, which replaces outgoing metadata) rather than in
  a decide-specific path: PRD §18 wants trace propagation REST → gRPC →
  RabbitMQ everywhere, and the gateway middleware already normalizes
  `X-Trace-Id` (generated when absent, ≤64 chars).
- Judgment calls open to override: entity = requisition (vs a step entity
  type); shared payload for step and terminal events; `trace_id` NOT NULL
  `''` when no trace; no tenant+entity index yet (no reader exists — add
  it with #42/#43/#47).

## Evidence

All against a scratch PostgreSQL 17 on the local instance (database
`eproc_test_31`, throwaway `eprocurement` role with CI-parity privileges,
same harness as dev-note 030; packages run sequentially or with migrations
pre-applied because `dbmigrate` has no advisory lock):

- `make gen-proto` — regenerated `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}` cleanly; only procurement files changed.
- `go vet ./... && go build ./...` — clean.
- `TEST_DATABASE_URL=… go test ./services/procurement/outbox/ -count=1` —
  green: column round-trip (version default 1, published_at NULL, jsonb
  payload, tenant-scoped insert) and the failure path (missing-field guard;
  a later error in the same transaction rolls the appended row back).
- `TEST_DATABASE_URL=… go test ./services/procurement/approval/ -count=1` —
  green: the #30 domain + store suites unchanged, plus
  `TestStoreDecideIdempotencyAndOutbox` (mid-flow keyed decide → 1 row
  `pr.step_approved` with actor + `trace-31-store` + full payload; replay →
  winner's current state, still 1 row, 1 key row; key on the other step →
  `ErrIdempotencyKeyConflict`, no writes; terminal decide → 3 rows incl.
  `pr.approved`; no-key duplicate → `ErrStepNotCurrent`, no growth;
  cross-tenant keyed decide → `ErrStepNotFound`; reject → 2 rows
  `pr.step_rejected` + `pr.rejected`) and `TestStoreDecideConcurrentSameKey`
  (2 goroutines, same step, same key → both succeed with identical
  decision, exactly 2 outbox rows, 1 key row).
- `TEST_DATABASE_URL=… go test ./services/procurement/requisition/
  -count=1` — green (unchanged suites against the 0006-extended schema).
- `TEST_DATABASE_URL=… go test ./services/procurement/server/ -count=1` —
  green, including `TestApprovalGRPCDecideIdempotencyOutbox`: 65-char key →
  `InvalidArgument approval_idempotency_key_invalid` with 0 outbox rows;
  keyed decide with `x-trace-id: trace-grpc-31` → row
  `pr.step_approved`, `actor_id` = decider, `trace_id` = `trace-grpc-31`;
  replay identical; key on step 2 → `AlreadyExists
  approval_idempotency_key_conflict`; terminal decide → 3 rows.
- `go test ./services/gateway/... -count=1` — green, including
  `TestApprovalDecideIdempotencyKey` (forwarded trimmed; blank/absent →
  empty key; >64 → 400 `approval_idempotency_key_invalid` without a
  procurement call) and `TestApprovalDecideTracePropagation`
  (`X-Trace-Id: trace-edge-31` → outgoing `x-trace-id` metadata; absent →
  a generated 32-hex id rides along).
- `go test ./services/procurement/... -count=1 -p 1` on the cleaned
  database — all packages green in one sequential run.
- `psql \d procurement.outbox` + `schema_migrations` listing — 0006 applied
  with the documented columns, partial index, and forced RLS policy.
- Pre-existing, unchanged: `TestVendorGRPCList` fails on repeat runs
  against one dirty database (dev-note 029 leak); running `go test` over
  several DB-backed packages in parallel races the migration apply
  (dev-note 030 `dbmigrate` note) — sequential `-p 1` or pre-applied
  migrations (what CI does) avoid both.
- pnpm build/lint/test not run by me (no TypeScript touched); the workflow
  script re-runs the full gate set after this dev note.
- No compose/seed changes.

## Follow-ups

- **#45 re-sequenced**: publisher loop only — poll (2 s, batch 50),
  assemble the §14.5 envelope from the columns, publish to the
  `procurement.events` topic exchange, mark `published_at`, retries + DLQ.
  The PR description flags the re-sequencing and the §14.6 catalog
  extension for maintainer attention.
- #42/#43/#47: timeline/events read path consuming
  `entity_type`/`entity_id`; add the tenant+entity index with the first
  reader.
- Submit/create requisition events are still in-memory only; wiring them
  into the outbox (`pr.submitted`, §14.6) belongs to #45 or a later issue.
- Reconcile the #30 in-memory aggregate event log with the outbox rows
  (consume `PullEvents()` in the store or drop the aggregate log).
- The notification consumer (#45 worker) uses `payload.requester_id` as
  the recipient.
- Pre-existing (from dev-notes 029/030, untouched): the procurement server
  test cleanup leak; `dbmigrate` advisory lock; `go test -race` unrunnable
  locally (no cgo).
