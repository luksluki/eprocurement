# 046 — RabbitMQ topology with DLX and retries

- **Issue:** #46
- **Branch:** feature/46-rabbitmq-topology-dlx-retries
- **Date:** 2026-10-25

> **Provenance:** like #45, prepared without maintainer input (no comments on
> the issue). The routing/binding conventions below follow PRD §14.5–§14.8
> as the design authority; the one judgement call in them — the
> notifications subset — is called out in Decisions and is open to override
> in PR review.

## What

- **`services/worker/topology` (new)** — the broker topology, declared once
  and idempotently (`Topology.Declare`, production values in
  `topology.Default`):
  - durable topic exchanges `procurement.events` (publisher target, routing
    key = event type §14.6) and `procurement.dlx` (dead-letter destination);
  - durable main queues `documents.history` / `notifications` / `exports`,
    each with `x-dead-letter-exchange: procurement.dlx` (§14.5), bound to
    the events exchange per the routing convention: history `#`, exports
    `export.requested`, notifications the actor-facing subset
    `pr.submitted`/`pr.approved`/`pr.rejected`/`po.issued`/
    `po.acknowledged`/`invoice.discrepancy`;
  - one TTL-backoff retry ladder per main queue: `<queue>.retry.1/2/3`,
    durable, `x-message-ttl` 5 s / 30 s / 5 min, empty
    `x-dead-letter-exchange` (default exchange) + `x-dead-letter-routing-key`
    = the main queue's own name, so a parked message returns to its own
    queue only;
  - durable `procurement.dlq` bound `#` to `procurement.dlx` — the final
    destination, replacing #45's PROVISIONAL bootstrap with
    argument-identical DLX/DLQ declarations (no PRECONDITION_FAILED
    possible on a broker that carries the #45 pair).
  - the consumer-side failure helper, defined once for the #47 consumers:
    `NextRetry(queue, retryCount)` (tier `retryCount+1` for attempts 1–3,
    ok=false on the 4th failure → the caller rejects without requeue and
    the queue's DLX argument routes to the DLQ), plus `RetryCount(headers)`
    (tolerant `x-retry-count` reader — AMQP integer widths decode
    differently across clients) and the `MaxRetries`/`RetryCountHeader`
    constants.
  - `Topology.Suffixed` / `QueueNames` / `Delete` exist for the tests
    (isolated per-run names + cleanup); production never deletes broker
    topology.
- **`services/worker/publisher/publisher.go`** — the local topology
  constants and the PROVISIONAL declarations in `connect` are gone; connect
  now calls `topology.Default.Declare(ch)` (idempotent, boot and every
  reconnect) and `Publish` targets `topology.EventsExchange`. The DLX/DLQ
  wire arguments are unchanged from #45.
- **`services/worker/publisher/publisher_test.go`** — the end-to-end test's
  passive checks now cover the full topology (both exchanges + every queue
  from `Default.QueueNames()`: mains, all nine retry tiers, DLQ); the
  "no consumer queues exist yet" comment is updated; the test purges the
  production queues at the end, because since this issue its published
  messages (and any other rows riding the batch) land in
  `documents.history`/`notifications`, which nothing consumes until #47.
- **`docs/schema/procurement.md`** — the PROVISIONAL DLX bullet is replaced
  by a pointer to a new "RabbitMQ topology (#46)" section documenting the
  exchanges, the routing/binding convention, the retry ladder mechanics,
  and the max-attempts failure path.

## Decisions

- **Notifications takes an actor-facing subset, history takes `#`.** §14.5
  names the three queues but not their bindings; §14.6 is the catalog. The
  subset (everything a human recipient is notified about per the PRD
  flows — requisition decided, PO issued/acknowledged, invoice discrepancy;
  deliberately not `vendor.created`, `grn.posted`, `invoice.matched` or the
  export lifecycle) is this issue's one judgement call, flagged for
  maintainer override in review. `documents.history` is the projection of
  record, so it binds `#`.
- **Max attempts = 4 deliveries** (first + 3 retries), then the DLQ — the
  smallest reading of "after max attempts" against the PRD's fixed three
  TTL tiers; the count travels in `x-retry-count`, bumped by the consumer
  on every retry, so the broker needs no plugin and the DLQ copy shows the
  final count.
- **Retry returns via the default exchange with an explicit routing key**
  (`x-dead-letter-exchange: ""`, `x-dead-letter-routing-key: <main queue>`)
  rather than re-publishing to the events exchange: the redelivery is queue
  topology, not a new domain event — routing keys stay event types, and one
  queue's retry can never be fanned out to another queue's binding.
- **The topology is declared by the publisher's connect** (as in #45) rather
  than by each consumer: boot order stays irrelevant — whoever connects
  first declares, declares are idempotent, and consumers (#47) reuse
  `topology.Default` unchanged. The #45-wired boot/reconnect paths are
  already in place.
- **Tests isolate with `Suffixed` names on the shared dev broker** (own
  exchanges + queues per run, deleted on cleanup): the routing AC is about
  the binding table, and parallel DB-backed suites publish real rows to the
  production exchange through the running worker, so asserting exact queue
  contents on production names would be flaky. Production-name correctness
  is pinned by the `Default` golden unit test and the publisher e2e passive
  checks; broker routing is proven on the isolated clone (same code path,
  different names).
- **AC2 uses short TTLs (150 ms tiers) plus one real 800 ms hop**: the
  cycle test proves the full 4-delivery arc (retry tiers 1→2→3, each
  returning to its own queue, 4th failure in the DLQ with `x-retry-count`
  = 3 and the rejected x-death trail) quickly, and the dedicated TTL test
  proves the delay genuinely belongs to the broker (nothing returns before
  the tier TTL, then arrives with the expired x-death trail and the
  default-exchange routing key override).
- **No CI change**: broker tests follow the `TEST_RABBITMQ_URL` skip-empty
  pattern of #45; CI has no RabbitMQ service.

## Evidence

All against the local compose stack (RabbitMQ 3.13 on :5672, PostgreSQL 17
on :55432, MongoDB on :27017), commands run from the repo root:

- `go vet ./... && go build ./...` — clean. `gofmt` clean on the new
  topology files (the `gofmt -l` listing of the pre-existing files on this
  Windows checkout is the CRLF working-copy state noted in dev-note 045,
  verified identical via `git stash`).
- `TEST_DATABASE_URL=… TEST_RABBITMQ_URL=… TEST_MONGO_URI=… go test ./…
  -count=1` — **exit 0, 24 packages ok, no failures, run twice in a row**
  (the second run doubles as the dirty-database repeatability check; the
  dev-note 029/030 flakiness did not appear). Includes the new suites:
  - `./services/worker/topology/`: `Default` golden (names, bindings, TTL
    ladder), `NextRetry` decision table incl. the exhausted/corrupt-count
    failure paths, tolerant `RetryCount` header decoding, queue-argument
    contract (exact `amqp.Table`s — the PRECONDITION_FAILED guard),
    `Suffixed`/`QueueNames`, bad-TTL declare rejection; broker-backed
    `TestEventRouting` (AC1: pr.submitted → history+notifications,
    vendor.created → history only, export.requested → history+exports,
    grn.posted → history only; retry tiers and DLQ proven empty; the
    second declare proves idempotence), `TestFailedDeliveryEndsInDLQ`
    (AC2: 4 deliveries then DLQ, isolation — history keeps exactly the
    initial copy, notifications nothing, tiers empty),
    `TestRetryTierWaitsOutTTL` (real 800 ms hop: nothing before the TTL,
    x-death expired trail + routing-key override after).
  - `./services/worker/publisher/`: all #45 suites unchanged-green plus
    the extended `TestAMQPPublisherEndToEnd` passive checks over the full
    topology and the production-queue purge.
- Broker left clean by the tests: `docker exec eprocurement-rabbitmq-1
  rabbitmqctl list_queues name messages` shows no suffixed test queues.
  It does show real messages in `documents.history`/`notifications` —
  published by the **running compose worker** (still the #45 build) from
  outbox rows the parallel suites wrote into the shared dev database; with
  the queues now existing, the dev stack buffers them until #47's
  consumers arrive (§14.8 "queue retains messages"). Expected, noted as a
  follow-up below.
- pnpm gates: not run by me (no TypeScript touched); the workflow script
  runs them after this note. compose/seed untouched (no compose or seed
  changes in this diff).

## Follow-ups

- **#47 consumers** consume `documents.history`/`notifications`/`exports`
  reusing `topology.Default.Declare` and the `NextRetry`/`RetryCount`
  failure helper; they will drain the dev-stack backlog described above.
- **#48** is the DLQ admin/monitoring surface; the DLQ depth metric is #54.
- The notifications subset and the 4-delivery max-attempts reading are the
  maintainer-override candidates flagged in Decisions; the PR description
  should repeat them.
- Reconcile the in-memory aggregate event logs with outbox rows (pre-
  existing #31 follow-up, unchanged).
