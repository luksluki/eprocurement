# 047 — Worker consumers with Mongo idempotency store

- **Issue:** #47
- **Branch:** feature/47-worker-consumers-idempotency
- **Date:** 2026-10-25

> **Provenance:** the three open questions of #47 were answered by the
> maintainer in the issue comments before implementation: (1) the consumer
> effect is a Mongo `notifications` collection — one document per event,
> unique `event_id`, recipient from `payload.requester_id` with
> `payload.submitted_by` fallback; (2) the §14.5 wire envelope is extended
> with `entity_type`/`entity_id` (the outbox columns already exist and
> `history.Event.Validate` already requires them); (3) the idempotency
> store is the Mongo unique index (PRD §14.3), not Redis SETNX. PRs #94
> (#45) and #95 (#46) were still unmerged, so this is built self-contained
> against develop with minimal textual overlap, per the issue comment.

## What

- **`services/documents/notifications` (new)** — the notification
  projection: `Store.Apply(ctx, history.Event) (applied, err)` inserts
  exactly one document into MongoDB `notifications` (`uniq_event_id`
  unique index = the dedupe; duplicate-key is a successful replay), with
  `recipient_id` derived as `payload.requester_id` → fallback
  `payload.submitted_by` → empty (document still created). Validation runs
  through the shared `history.Event` contract, so its sentinel errors
  (`ErrEventIDInvalid`/`ErrEventIncomplete`/`ErrPayloadInvalid`) surface
  unwrapped and the consumer can classify them as permanent. Pinned
  indexes `uniq_event_id {event_id:1}` unique + `tenant_occurred
  {tenant_id:1, occurred_at:-1}` (PRD §14.7). Documented in
  `docs/schema/documents.md`.
- **`services/worker/consumer` (new)** — the consumer runner:
  - **`envelope.go`** — the §14.5 envelope + `entity_type`/`entity_id`
    (field-for-field the #94 `publisher.Envelope`; self-contained copy,
    see Decisions), `Decode` (any decode failure is the typed
    `ErrEnvelopeInvalid`) and `Envelope.Event()` via `history.NewEvent`
    (normalize + validate). `isPermanent` maps the history validation
    sentinels to "dead-letter, never retry".
  - **`retry.go` + `topology.go`** — the retry ladder and topology
    declarations, inline copies of PR #95's
    `services/worker/topology/topology.go` pinned argument-for-argument:
    `x-retry-count` header, `MaxRetries=3`, TTL ladder 5 s → 30 s → 5 min,
    `NextRetry`/`RetryCount` (tolerant to every AMQP integer width, 0 when
    absent), `RetryQueueName {queue}.retry.{n}`, main queues with
    `x-dead-letter-exchange: procurement.dlx`, retry queues with
    `{x-message-ttl, x-dead-letter-exchange: "", x-dead-letter-routing-key:
    <main queue>}`, durable `procurement.events` + `procurement.dlx`
    exchanges, `procurement.dlq` bound `#` to the DLX,
    `documents.history` bound `#` and `notifications` bound the exact six
    `NotificationsRoutingKeys`. The consumer-scoped subset (no exports
    queue); re-declaration after #45/#46 merge cannot raise
    PRECONDITION_FAILED.
  - **`consumer.go`** — `Runner.Run(ctx)`: dial + declare + `Qos(prefetch
    10)` + consume both queues over one channel, dispatch serially;
    per-delivery outcome logged as `status` `applied` / `duplicate` /
    `retry-tier` / `dlq` with `event_id`, `tenant_id`, `type`, `queue`,
    `trace_id` (from the envelope). Transient handler failures (bounded by
    a 10 s per-delivery timeout) publish a copy to the next retry tier via
    the default exchange with the header raised, then ack the original;
    when the ladder is exhausted the delivery is nacked without requeue
    and dead-letters to `procurement.dlq`. Permanently invalid messages
    (undecodable body, invalid envelope) skip the ladder and dead-letter
    immediately. Connection loss (or a broker that is not up yet) is
    logged and re-dialed with 1 s → 10 s backoff; ctx cancel (SIGINT/
    SIGTERM) lets the in-flight handler finish, closes the connection so
    unacknowledged deliveries return to their queues, and returns nil.
- **`services/worker/cmd/worker/main.go` (rewrite of the skeleton)** —
  config + logger + signal context; Mongo connect+**ping** with backoff
  until ready or shutdown (no crash-loop, PRD §14.8 "worker down");
  `history.Store.EnsureIndexes` + `notifications.Store.EnsureIndexes`
  **before** consumption starts (fresh databases dedupe from the first
  delivery); then `Runner.Run` on the two queues.
- **`services/worker/config/config.go` (new)** — `Env`, `LogLevel`,
  `RabbitMQURL`, `MongoURI`, `MongoDB` with local defaults mirroring the
  documents service and compose credentials. No new env knobs: the retry
  TTLs are the PRD-pinned 5 s/30 s/5 m and are injectable only through
  `Topology.RetryTTLs` (tests).
- **`internal/platform/config` deleted** — its only user in develop was
  the worker skeleton (verified `git grep` over `*.go`; same removal as
  #45's commit 48d65a6 did on its branch).
- **`go.mod`/`go.sum`** — `github.com/rabbitmq/amqp091-go v1.16.0` added
  (the only new dependency, identical to #45/#46).
- **`docs/schema/documents.md`** — new "Notifications projection (#47)"
  section: collection fields, recipient rule, pinned indexes, Apply
  semantics.

## Decisions

- **Self-contained against develop, swap recorded**: #94/#95 were unmerged
  at implementation time, so the envelope type and the topology/retry
  declarations are inline copies (the maintainer's issue comment asked for
  exactly this). When #46 merges, `retry.go`/`topology.go` are deleted and
  call sites switch mechanically to `topology.Default.Declare`,
  `topology.NextRetry`, `topology.RetryCount`, `topology.RetryQueueName`,
  and `NotificationsRoutingKeys`; when #94 merges, `envelope.go` collapses
  onto `publisher.Envelope`. The declarations are identical
  argument-for-argument, so a broker declared by either PR accepts the
  other's re-declare (no PRECONDITION_FAILED).
- **Permanent vs transient** is decided by error identity, not by the
  broker: decode failure, envelope-validation failure, and the history
  validation sentinels bubbling out of a store are permanent (zero
  retries, straight to DLQ — they can never succeed); everything else
  (Mongo down, network, ctx deadline) is transient and climbs the ladder.
  The envelope is validated *before* the handler runs, so an event without
  `entity_type` dead letters before Mongo is touched.
- **Serial dispatch, one channel, prefetch 10**: deliveries are handled
  one at a time per runner, which keeps queue order, makes shutdown
  trivially correct (at most one in-flight handler), and is ample for the
  demo load; the retry ladder parallelizes recovery across messages anyway.
- **Retry-copy publish failure re-queues the original** (`nack requeue=true`)
  rather than dead-lettering: losing the ability to park a retry must not
  lose the message. The redelivery climbs the ladder with the header it
  still carries.
- **Ack/Nack errors ignored after settle**: once the connection is gone
  they always fail; the broker then redelivers and the store dedupes —
  that is the at-least-once contract (PRD §14.8), not an error path.
- **Indexes before consumption** (cmd/worker boot order) and the
  integration tests prove it the same way: every test gets a fresh scratch
  database whose indexes come from the same `EnsureIndexes` calls boot
  makes, then dedupe is asserted on it.
- **DLQ metric deferred to #54** (PRD §14.5 "DLQ + metric"): the repo has
  no observability stack yet (compose's observability profile is parked,
  same finding as dev-note 045); the per-delivery structured log with
  `status=dlq` is the interim visibility, and `procurement.dlq` is
  admin-visible through RabbitMQ's management surface.
- **Post-merge composition — DONE in this branch's merge of develop**
  (PRs #94/#95 both landed; resolved 2026-10-23):
  1. ~~one binary, two runners~~ — `cmd/worker` now boots the publisher
     half (pgxpool + `connectWithRetry` → `publisher.Loop`) and the
     consumer half (`connectMongo` + `EnsureIndexes` → `consumer.Runner`)
     and runs both under one signal context via `runBoth`: SIGINT/SIGTERM
     cancels a derived context, both `Run` methods settle their in-flight
     work and return nil; a non-nil error from either half is logged,
     cancels the sibling, and fails the process (`publisher: …` /
     `consumers: …` wrapping). Shutdown during boot returns nil, not 1.
     `services/worker/config` is the union Config (DATABASE_URL + poll
     knobs + MONGO_URI/MONGO_DB; RABBITMQ_URL shared).
  2. ~~compose `MONGO_URI` follow-up~~ — already satisfied: develop's
     `worker` service (via #97's compose) ships `MONGO_URI`/`MONGO_DB` and
     `depends_on: mongo (service_healthy)`; nothing to add.
  3. ~~`internal/platform/config` removal~~ — no-op as predicted (both
     sides had deleted it).
  In the same merge the inline copies were dropped as planned:
  `consumer/topology.go` + `consumer/retry.go` deleted in favor of
  `services/worker/topology` (`topology.Default.Declare`,
  `topology.NextRetry`/`RetryCount`/`RetryQueueName`/`RetryCountHeader`,
  `topology.DocumentsHistoryQueue`/`NotificationsQueue`) — the shared
  `Default` additionally declares the `exports` queue, which the runner
  declares but does not consume until its own issue; and
  `consumer/envelope.go` deleted in favor of `publisher.Envelope` — the
  package keeps only `Decode`, the free `Event(env)` helper (the old
  method could not attach across packages), `ErrEnvelopeInvalid`, and
  `isPermanent`, now living in `consumer.go`. The consumer unit tests that
  duplicated the topology package's own tables (NextRetry/RetryCount/
  TTLs/queue-args/Default/Suffixed/Declare-guard) were deleted; the
  envelope-decode, envelope-validation, wiring, and shutdown tests remain,
  now typed against `publisher.Envelope`.
- **Test-side finding worth keeping**: readiness polling must wait for the
  whole declared queue set, not just the exchange or the main queues — the
  runner declares an exchange, then each queue, then that queue's bindings,
  then its retry ladder, and a topic exchange silently drops unroutable
  messages. A publish in any of those windows vanishes (caught as a 15 s
  timeout with zero consumer activity under parallel-suite load;
  `waitReady` now passive-declares every `QueueNames()` entry — the ladder
  is declared after the bindings, so its existence proves them). Test
  topologies are deleted in cleanup so the dev broker stays clean.

## Evidence

All commands run from the repo root against the local stack (RabbitMQ via
compose on :5672, MongoDB on :27017):

- `go vet ./... && go build ./...` — clean. `gofmt` clean on every file
  this PR touches (the repo-wide `gofmt -l` listing is the pre-existing
  CRLF working-copy state, identical on develop — dev-note 045).
- `TEST_MONGO_URI=… go test ./services/documents/notifications/ -count=1`
  — green: recipient table (requester_id wins / submitted_by fallback /
  blank falls back / absent / non-object / invalid JSON), Apply
  idempotency (one document, replay `applied=false`), stored-shape table
  (recipient-less document still created, `recorded_at` set), validation
  failures (missing entity_type/entity_id/tenant_id/event_id, invalid
  payload → history sentinels, zero documents written), and the pinned
  index set listed from a fresh database (`uniq_event_id` unique +
  `tenant_occurred`, exactly three indexes incl. `_id_`).
- `TEST_RABBITMQ_URL=… TEST_MONGO_URI=… go test
  ./services/worker/consumer/ -count=1` — green, and re-run three times
  consecutively, all green (~4.8 s each). Covers unit tests (NextRetry
  boundaries incl. `-1`, RetryCount over int8/int16/int32/int64/int/absent/
  garbage, TTL pin 5 s/30 s/5 m, queue-args pin, Default topology + queue
  names + the six notification routing keys, Suffixed isolation, envelope
  decode/validation tables, New validation, Run-returns-nil against a
  closed port) and the broker+Mongo integrations:
  - `TestConsumerEndToEndIdempotent` — `pr.approved` flows to events +
    audit_log + notifications; the duplicate delivery of the same
    `event_id` produces no second document anywhere (a later event
    processing behind it proves the duplicate was acked); recipient
    `user-ev-approved-1` from `requester_id`, `user-2` from `submitted_by`
    fallback; both queues drain to 0.
  - `TestRetryLadderThenSuccess` — 2 scripted transient failures then
    success: exactly 3 handler attempts, event applied once, ladder and
    DLQ empty.
  - `TestRetryExhaustionDeadLetters` — always-failing handler: exactly
    `MaxRetries+1` = 4 attempts, original body found in the (suffixed)
    DLQ, all retry queues empty, nothing projected.
  - `TestPermanentInvalidGoesStraightToDLQ` — undecodable body and an
    envelope without `entity_type` both land in the DLQ while the valid
    event behind them is applied (no head-of-line blocking).
  - `TestShutdownRedeliversWithoutDoubleEffect` — ctx cancel with a
    delivery held mid-handler: `Run` returns nil, the unacknowledged
    delivery returns to its queue, and a restarted runner applies it
    exactly once.
- `go test ./...` (without TEST_* vars) — the new suites skip cleanly
  (integration) / pass (unit), matching the repo convention; CI is not
  changed and has no RabbitMQ/Mongo service for these.
- pnpm build/lint/test: **not run** — no TypeScript is touched by this PR
  (same honest-reporting convention as dev-note 045).
- compose/seed/Makefile/CI: untouched (`git status` confirms).

## Follow-ups

- ~~**After #95 (#46) merges**~~: done in this branch's develop merge —
  `retry.go`/`topology.go` deleted, consumer switched to
  `topology.Default.Declare` + the topology retry helpers, integration
  suite re-run green against the shared production topology (which now
  also declares `exports`).
- ~~**After #94 (#45) merges**~~: done in the same merge — envelope folded
  into `publisher.Envelope`; publisher Loop and consumers composed in one
  `cmd/worker` under one signal context; compose `MONGO_URI` +
  `depends_on: mongo` verified already present on develop (no change
  needed).
- **#54**: DLQ metric (PRD §14.5 "DLQ + metric"), OTel spans for the
  consume path, and trace-context propagation into AMQP headers — deferred
  with the observability stack, same as #45.
- Read side for notifications (a future issue decides what surfaces them);
  the collection and its `tenant_occurred` index are ready for it.
- PR description must flag: the maintainer-approved envelope extension
  (`entity_type`/`entity_id`, already patched into #94), the inline
  topology duplication until #46 merges, the compose `MONGO_URI` follow-up,
  and the deferred DLQ metric.
