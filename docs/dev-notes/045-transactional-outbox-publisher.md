# 045 — Transactional outbox and publisher

- **Issue:** #45
- **Branch:** feature/45-transactional-outbox-publisher
- **Date:** 2026-10-25

> **Provenance:** the maintainer was unavailable while #45 was prepared, so
> the open infrastructure questions were decided by the agent on 2026-10-25
> ("Decisions applied by agent 2026-10-25, maintainer unavailable — override
> welcome in PR review" — issue #45 comment 2; comment 1 parks the issue for
> a maintainer answer that never came; neither is a maintainer approval).
> The applied decisions, all open to override in PR review and flagged in
> the PR description for CODEOWNERS review (@luksluki):
>
> 1. **Dependency `github.com/rabbitmq/amqp091-go` v1.16.0** — the only
>    official RabbitMQ client for Go; PRD §14/ADR-0001 already mandate
>    RabbitMQ, so this implements an existing architecture decision rather
>    than introducing a new one.
> 2. **Worker service in `compose.yaml`** (DATABASE_URL → postgres,
>    RABBITMQ_URL → rabbitmq, no exposed ports) — compose's own comment
>    anticipated the worker joining; the change is additive.

## What

- **`services/procurement/outbox/pending.go`** (new) — the publisher's
  persistence side: `Pending(ctx, db, limit)` returns unpublished rows
  oldest-first (created_at, id) across ALL tenants through the
  transaction-scoped `app.rls_bypass` arm; `MarkPublished(ctx, db, ids)`
  stamps `published_at` under a `published_at IS NULL` guard (re-marking
  never overwrites the first timestamp) and nothing is ever deleted. Lives
  with the table's owning service (procurement), executed by the worker —
  a conscious cross-service import inside the single Go module.
- **`services/procurement/requisition/store.go`** — Submit now appends
  exactly one `pr.submitted` outbox row in the same transaction as the
  draft → submitted UPDATE and the step materialization
  (`appendSubmittedTx`): one row on success, zero on idempotent replay
  (early return before the write) and zero on any rollback (missing
  approval rules, empty basket, illegal transition). Payload is the pinned
  public contract `{requisition_id, number, requester_id, currency,
  total_cents, submitted_by, submitted_at}` — snake_case per the
  DecisionPayload convention; `requester_id` is what the future
  notification consumer reads as recipient (dev-note 031). `actor_id` =
  the submitting caller, `trace_id` = `logger.TraceID(ctx)`.
- **`services/worker`** (real behaviour replacing the skeleton print):
  - `config` — env with pinned defaults: poll 2 s / batch 50 (PRD §16),
    confirm timeout 5 s; overrides (`WORKER_POLL_INTERVAL`,
    `WORKER_BATCH_SIZE`, `WORKER_CONFIRM_TIMEOUT`) are for tests/local
    tuning; compose does not set them.
  - `publisher` — §14.5 envelope `{event_id, tenant_id, type, version,
    occurred_at, actor_id, trace_id, payload}` assembled 1:1 from the row
    columns (the #31 schema deltas exist for exactly this); `AMQPPublisher`
    dials with a 10 s timeout, declares the durable topic exchange
    `procurement.events` + the PROVISIONAL `procurement.dlx`/`procurement.dlq`
    pair (see Decisions), puts the channel in confirm mode, publishes with
    routing key = event type, `delivery_mode` persistent, `message_id` =
    event_id, and waits for the broker confirm (bounded by the confirm
    timeout; a timeout drops the connection so the next publish reconnects).
    `Loop` polls once at boot then every interval; every poll or publish
    failure is non-fatal — warn with the row's `event_id`/`tenant_id`/
    `event_type`/`trace_id`, next tick retries; a failing row stops the
    batch so ordering is preserved and the tail retries.
  - `cmd/worker/main.go` — config + logger + pgxpool + RabbitMQ connect
    with exponential backoff (1 s → 10 s cap) until shutdown; SIGINT/SIGTERM
    cancels the context and `Loop.Run` returns. The worker runs NO
    migrations — procurement stays the sole applier (dev-note 030's
    no-advisory-lock dbmigrate race), which is exactly why poll errors must
    be non-fatal: on a fresh volume the worker boots (postgres + rabbitmq
    healthy) before procurement has applied migration 0006.
- **`compose.yaml`** — `worker` service: build from the shared Dockerfile
  (`SERVICE: worker`), DATABASE_URL → postgres, RABBITMQ_URL → rabbitmq,
  no ports, no healthcheck (nothing listens), `depends_on` postgres +
  rabbitmq healthy only (deliberately NOT procurement — see Decisions).
  The stale "remaining application services" comment is gone.
- **`.env.example`** — documents the three WORKER_* overrides as
  test/local-only knobs.
- **`docs/schema/procurement.md`** — publisher section under Migration 0006
  (mechanics, bypass rationale, envelope, PROVISIONAL DLX label,
  observability deferral to #54), the `pr.submitted` payload contract, the
  create-event-stays-in-memory note (§14.6 has no create event name), the
  flagged numbering mismatch, and the follow-ups list.

## Decisions

- **Poll/publish errors are log-and-continue, not fatal** (AC "publisher
  retries safely"): a fatal loop would kill the container on a fresh
  `make reset && docker compose up -d --wait` volume — the worker's
  `depends_on` cannot include procurement (that would still race: DB
  restarts and migration windows are transient too), so tolerance is the
  robust choice. Tested against a real unmigrated database (see Evidence).
- **No `depends_on: procurement`** for the worker: ordering by dependency
  would not remove the failure window (procurement healthy ≠ migrations
  visible to a restarting DB), couples boot order, and procurement already
  gates the gateway. The loop's tolerance covers both boot and runtime.
- **Mark-only-the-confirmed-prefix, stop the batch on first failure**:
  preserves publish order and bounds a broker outage to one retry per tick.
  Rows already confirmed but unmarked (crash between publish and mark) are
  republished — at-least-once by design; consumers dedupe on `event_id`
  (§14.8; the Mongo event store of #42 already implements that dedupe).
- **PROVISIONAL DLX declaration** at connect (durable `procurement.dlx` +
  `procurement.dlq` bound `#`): the full §14.5 model is consumer-side
  dead-lettering (per-queue `x-dead-letter-exchange` + TTL retry queues
  5 s → 30 s → 5 m) and arrives with the consumer issues; this bootstrap
  only gives dead letters a destination and admin visibility. Labeled as
  provisional in code and docs; expected to be reshaped in review.
- **`pr.submitted` payload is DIBIN as a public cross-issue contract**
  (snake_case like DecisionPayload) rather than an ad-hoc struct, because
  the notification consumer (a later issue) reads `payload.requester_id`.
- **Create/draft events stay in-memory deliberately**: the §14.6 catalog
  has no create event name (`pr.submitted` is the first catalog step), so
  inventing one is a catalog decision, not a publisher wiring — documented
  two-ways in the schema doc as the plan required.
- **Observability deferred to #54 explicitly** (AGENTS asks for spans/
  metrics on new jobs; §18 lists outbox lag): the repo has zero OTel/
  Prometheus code (grep over internal/ services/ scripts/ matches nothing)
  and compose's observability profile is parked for #54; #45 ships the
  structured publish log (event_id, tenant_id, event_type, trace_id) and
  records AMQP trace-header propagation + the lag metric as #54 follow-ups.
- **Cross-tenant read is the publisher's sanctioned exception**: Pending/
  MarkPublished use `app.rls_bypass`; writer-side RLS stays enforced and
  the multi-tenant test also proves a tenant-scoped read still sees only
  its own rows (the AGENTS tenancy convention holds where it applies).
- **Test-side**: the approval and server suites now filter their decide
  event counts by event type (the same requisition carries `pr.submitted`
  since this PR); the publisher tests never assume a globally clean shared
  database — Pending reads every tenant and the DB-backed suites run in
  parallel against one database, so the AMQP test quarantines pre-existing
  unpublished rows at start and fetches its message by `message_id`
  (skipping other suites' rows that legitimately ride the batch), and the
  Pending tests assert on specific ids instead of global counts.

## Evidence

All against the local stack (PostgreSQL 17 via compose on :55432, scratch
databases `eproc_test_45` + per-test `eproc_test_45_scratch`; RabbitMQ via
compose on :5672); commands run from the repo root:

- `go vet ./... && go build ./...` — clean. `gofmt` clean on every touched
  file (the `gofmt -l` mass listing on this Windows checkout is the
  pre-existing CRLF working-copy state, identical on develop).
- `TEST_DATABASE_URL=… TEST_MONGO_URI=… TEST_RABBITMQ_URL=… go test ./…
  -count=1` — **all packages green in one parallel run** (the CI shape),
  including the DB + RabbitMQ integrations below.
- `TEST_DATABASE_URL=… go test ./services/procurement/outbox/ -count=1` —
  green: #31 round-trip/guard suites plus the new crash simulation
  (AppendTx commit, no marking → Pending returns the row on consecutive
  polls; MarkPublished removes it from Pending, row never deleted,
  re-mark keeps the first timestamp), cross-tenant batch (one Pending call
  drains rows of ≥2 tenants in commit order; under tenant A's scoped read
  tenant B's row is invisible while the bypass sees it — the bypass, not a
  broken policy), limit 1 bounds the batch, and the non-positive-limit
  guard. Assertions match specific ids instead of global counts because
  Pending reads every tenant and the DB-backed suites run in parallel
  against one test database.
- `TEST_DATABASE_URL=… go test ./services/procurement/requisition/ -count=1`
  — green: existing suites plus `TestStoreSubmitOutboxEvent`: rollback
  (failing materializer) → 0 rows; successful submit → exactly 1
  `pr.submitted` row with the full payload contract, actor, `trace-45-submit`,
  published_at NULL; idempotent replay → still 1; resubmit →
  ErrIllegalTransition, still 1; empty basket → ErrItemsEmpty, 0 rows.
- `TEST_DATABASE_URL=… go test ./services/procurement/approval/
  ./services/procurement/server/ -count=1 -p 1` — green with the decide-type
  filter (the #30/#31 behaviour unchanged around the new submit row).
- `TEST_DATABASE_URL=… TEST_RABBITMQ_URL=… go test ./services/worker/
  publisher/ -count=1` — green (stable across repeated runs), covering:
  envelope golden JSON (exact §14.5 field names/order), retry semantics
  (failed publish leaves published_at NULL; next tick publishes + marks),
  batch stop on failure (tail retries in order after recovery), loop.Run
  surviving scripted poll errors, `TestLoopRunAgainstUnmigratedDatabase`
  (real scratch DB with NO migrations: the running loop polls into
  "schema does not exist" errors, then publishes and marks within seconds
  after the migrations are applied — the fresh-volume compose scenario),
  `TestAMQPPublisherEndToEnd` (real broker: passive checks prove
  procurement.events / procurement.dlx / procurement.dlq exist after
  connect; a committed row flows loop → confirm-mode publish → capture
  queue with routing key pr.submitted, delivery_mode 2, message_id =
  event_id, envelope body matching the row; row marked published; skips
  other suites' rows that legitimately ride the same batch), and
  `TestConnectFailure` (closed port → error). The AMQP tests skip cleanly
  without TEST_RABBITMQ_URL — CI has no RabbitMQ service and CI is not
  changed (golden rule 10).
- `docker compose config` — valid with the worker service.
- `docker compose build worker && docker compose up -d --wait &&
  docker compose ps` — green: the worker container builds from the shared
  Dockerfile, reaches `running`, joins the healthy stack, and its logs show
  the pinned loop (`poll_interval":"2s","batch_size":50`) ticking without
  errors. The destructive fresh-volume variant (`make reset`) was NOT run —
  it would wipe the maintainer's dev volumes; the fresh-volume boot order
  (worker before migrations, poll errors non-fatal, recovery after the
  migrations land) is proven by `TestLoopRunAgainstUnmigratedDatabase`
  against a real unmigrated database. The workflow script re-runs the
  full gate set (Go + pnpm) after this dev note.
- pnpm build/lint/test: not run by me (no TypeScript touched).
- Pre-existing, reproduced on develop (git stash) to confirm they are not
  caused by this diff: `TestVendorGRPCList` fails on repeat runs against
  one dirty database (dev-note 029 leak — fails with
  `vendor_duplicate_code` after earlier runs left rows behind), and
  `go test` over several DB-backed packages in parallel can race shared
  test data (dev-note 030).

## Follow-ups

- **#54**: OTel spans for the publish path, trace-context propagation into
  AMQP message headers, and the outbox lag metric (§18) — deferred
  deliberately (no observability stack exists yet).
- AMQP consumers (documents.history already dedupes by event_id in Mongo
  per #42; notifications reading `payload.requester_id`; exports), the
  consumer-side retry queues (5 s → 30 s → 5 m) and real DLQ semantics
  that supersede the PROVISIONAL DLX bootstrap declared here.
- Reconcile the requisition/approval in-memory aggregate event logs with
  the outbox rows (pre-existing #31 follow-up, unchanged).
- PR description must flag: the two agent-applied infrastructure decisions
  (amqp091-go dependency; worker in compose), the PROVISIONAL DLX, the
  pinned `pr.submitted` payload contract, and the re-sequencing context
  from #31.
- Pre-existing (dev-notes 029/030/031, untouched): procurement server test
  cleanup leak on repeat runs against one dirty database; dbmigrate has no
  advisory lock (run DB-backed packages sequentially or pre-apply
  migrations, as CI does); `go test -race` unrunnable locally (no cgo).
