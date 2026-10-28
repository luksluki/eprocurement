# 048 — DLQ monitor, metrics, and admin endpoint

- **Issue:** #48
- **Branch:** feature/48-dlq-monitor-admin-endpoint
- **Date:** 2026-10-28

> **Provenance:** the maintainer answered the issue's three questions on
> 2026-10-28 (issue #48 comments): (1) broker access lives in the gateway
> as a small AMQP client — **option A**, compose gateway gains
> `RABBITMQ_URL` + `depends_on rabbitmq (service_healthy)`; (2) replay
> resets `x-retry-count` to 0 (fresh 3-attempt ladder); (3) the route is
> `/api/v1/dlq`, the match-settings precedent, no `/admin` prefix. The
> Q&A does **not** close the *shape of the metric* — PRD §16's bullet
> names "hourly summary metric + admin endpoint" as two deliverables and
> PRD §18 lists DLQ depth among future Prometheus metrics — so this
> issue's reading (live depth in the admin response + hourly structured
> depth log in the worker; Prometheus/OTel export deferred to #54 with
> the observability stack, per dev-notes 045/047) is **flagged for
> maintainer sign-off in the PR body**, along with the decode_error
> cross-tenant decision below.

## What

- **`services/gateway/dlq` (new)** — the gateway's small AMQP client over
  `procurement.dlq` (option A). One process-wide mutex serializes
  List/Replay sessions; each session dials short-lived, bootstraps the
  DLX/DLQ/`#`-binding declarations (argument-identical to
  `topology.Declare`, constants imported from `services/worker/topology`),
  and reads `depth` from the `QueueDeclare` return **before** draining:
  - **List(ctx, tenant, limit)** drains the DLQ with `Basic.Get`
    (autoAck=false) holding up to **1000** messages (`DefaultScanCap`),
    parses each, requeues everything (`Nack(requeue=true)`) at the end,
    and returns `{Depth, Scanned, Messages}`. Decoded entries are
    filtered to the caller's tenant; **undecodable entries are returned
    cross-tenant as metadata only** (`decode_error: true`; message_id /
    original_queue / reason / died_at / retry_count — no payload, no
    tenant: the worker's `Decode` is a pure `json.Unmarshal`, so the
    tenant of a broken body cannot be known).
  - **Replay(ctx, tenant, messageID)** drains (same cap), finds the
    message by AMQP `MessageId` (= the publisher's `event_id`,
    publisher.go — a property, so it survives a broken body), applies the
    pure `replayDecision` gate, republishes to the original work queue
    (from `x-death`) via the default exchange with `x-retry-count`
    explicitly 0 and `x-death` dropped (body/MessageId/ContentType
    preserved), and acks the DLQ copy **only after** the publish
    succeeded — a failed publish requeues everything untouched (the
    consumer's `publishRetry` ack/nack pattern).
  - Typed errors: `ErrBrokerUnavailable` (503), `ErrMessageNotFound`
    (404), `ErrNotReplayable` (409).
- **`services/gateway/server/dlq.go` (new)** — `GET /api/v1/dlq?limit=`
  (default 50, max 500; the drain cap applies regardless) and
  `POST /api/v1/dlq/replay {"message_id"}`, both behind `bearerAuth` +
  `RequirePermission(rbac.DLQManage)`; local non-gRPC error mapping
  (400 `body_invalid` / `dlq_message_id_invalid`, 404, 409, 503).
  `server.go` gains the `WithDLQ` option and registers the routes;
  `gateway/config` gains `RabbitMQURL` (`RABBITMQ_URL`, default
  mirroring worker/config); `cmd/gateway` constructs
  `dlq.New(cfg.RabbitMQURL, topology.Default)` — dial-per-request, so a
  down broker costs those requests a 503, never boot.
- **`internal/platform/rbac`** — new permission **`dlq:manage`**
  (admin-only; matrix roles `nil`, the `approval:configure` /
  `match:configure` twin) **and the mandatory mirror
  `DLQManage: {}` in `wantPermission`** — without it
  `TestMatrixCoversEveryPermission` fatals `go test ./...`, which CI runs
  (docs/rbac.md "Adding or changing a permission", step 2).
  `docs/rbac.md` gains the table row and the numbered #48 note (route,
  tenant scope, replay 404/409 semantics).
- **`services/worker/dlqmonitor` (new)** — PRD §16's hourly summary:
  `Depth(ctx, url)` (short-lived dial + identical-argument DLQ bootstrap
  + ready count) and `Monitor.Run(ctx)` (tick immediately, then every
  `Interval`; per tick one structured `dlq depth summary` line — Info at
  0, Warn above; a failed tick is a warn, never fatal; injectable
  `Sink`/`depthFn`/`Topology` for tests).
- **`services/worker/config`** — `DLQSummaryInterval` ←
  `DLQ_SUMMARY_INTERVAL` (default 1 h, `durationOr` pattern).
- **`services/worker/cmd/worker`** — `runBoth` generalized to
  `runComponents` (N named components, identical error semantics: a
  non-nil error is logged, cancels the siblings, fails the process;
  shutdown during boot returns nil); the dlqmonitor is the third
  component and does **not** dial at boot.
- **`compose.yaml`** — gateway gains
  `RABBITMQ_URL: amqp://${RABBITMQ_USER:-eprocurement}:${RABBITMQ_PASSWORD:-eprocurement}@rabbitmq:5672/`
  and `depends_on: rabbitmq (service_healthy)` (verbatim from the
  maintainer decision). `.env.example` documents both.
- **Docs** — `docs/ops/dlq-monitor.md` (new runbook: inspection, replay,
  purge via broker tooling, WARN/CRITICAL thresholds, the 1000-scan cap
  and O(depth) cost, the cross-replica admin-tool race + retry guidance);
  `docs/schema/procurement.md` gains a "DLQ monitor (#48)" subsection
  under the RabbitMQ topology section plus the follow-up entry.

## Decisions

- **Undecodable entries are visible cross-tenant (metadata only), by
  design.** The tenant of a broken body cannot be known (decode is a
  pure `json.Unmarshal`), so tenant-filtering them is impossible: either
  every admin sees that a poison message exists, or nobody does. Chosen:
  visibility of poison messages > secrecy of their existence; the leak
  is existence-only (queue/reason/time/count, no payload, no tenant),
  documented in the runbook and `docs/rbac.md`. The flip side: such
  entries are **not replayable** (`409 dlq_message_not_replayable`) —
  replaying a permanently invalid message just bounces it back to the
  DLQ (consumer.go's dead-letter path); removal is broker tooling per
  the runbook. **Flagged for maintainer sign-off in the PR body.**
- **Depth is read before the drain, by construction.** `QueueDeclare`
  counts only ready messages; during the drain the tool holds them
  unacknowledged, so a post-drain read would report 0. The bootstrap
  declare doubles as the depth read; `scanned` makes the drain size
  (and the 1000 cap) transparent. Depth is point-in-time.
- **Replay matches on AMQP `MessageId`, not the decoded body** — it is
  the one identity that survives a broken body. Cross-tenant opacity:
  another tenant's decoded message answers 404 `dlq_message_not_found`,
  indistinguishable from unknown, and is untouched; a decoded but
  tenantless body (e.g. `{}`) is owned by nobody and also answers 404.
- **The admin-tool race is documented, not solved.** Within one gateway
  process a mutex serializes sessions; across replicas, a list/replay
  holding messages unacked makes them invisible to `Basic.Get` elsewhere
  (spurious 404/under-reported list). The runbook tells the operator to
  retry; correctness of the data itself is unaffected (requeue returns
  everything). A broker-level fix (peek without get) does not exist in
  AMQP 0-9-1.
- **Interim metric surface.** PRD §16's "hourly summary metric" is
  realized as the worker's structured depth log (hourly, `DLQ_SUMMARY_INTERVAL`);
  the admin endpoint's `depth` field is the live value. No metrics stack
  exists in the repo yet (compose's observability profile is parked —
  dev-notes 045/047), so Prometheus/OTel export lands with #54 (PRD §18
  lists DLQ depth). No new dependency; amqp091-go was already in go.mod.
- **Monitor tests use Suffixed topologies; the exported `Depth` is
  smoke-checked only.** The deterministic seeded-DLQ proof runs against
  a Suffixed topology (the repo's broker-isolation convention); the
  production-name `Depth` is called once for an error-free read without
  seeding, because seeding/purging the shared dev broker's real
  `procurement.dlq` (which held 367 real messages during verification)
  would race the running stack — the same reasoning as dev-note 046's
  Suffixed-isolation decision.
- **Alert thresholds are documented defaults, not code**: WARN depth ≥ 1
  persisting > 1 h (two consecutive nonzero hourly summaries — matches
  the cadence), CRITICAL depth ≥ 50 (runbook; override welcome).

## Evidence

All commands run from the repo root against the local compose stack
(RabbitMQ 3.13 on :5672; stack healthy):

- `go vet ./... && go build ./...` — clean. gofmt clean on every file
  this PR adds/edits (the repo-wide `gofmt -l` listing is the
  pre-existing CRLF working-copy state with `core.autocrlf=true` — index
  LF, working copies CRLF, identical on develop; noted in dev-notes
  045/046).
- `go test ./internal/platform/rbac/ -count=1` — ok (the new mirror
  passes `TestMatrixCoversEveryPermission` /
  `TestEveryRoleAgainstMatrix`).
- `go test ./services/gateway/server/ -count=1` — ok: DLQ routes RBAC
  (all five non-admin roles → 403 `{required: dlq:manage}` with the
  broker client untouched; no/garbage token → 401), list shape
  ({depth, scanned, messages}, decode_error entries carry no
  tenant/event fields, tenant from the claims, limit default 50 /
  cap 500 / garbage→default), replay forwarding + error mapping table
  (404/409/503) and 400 input paths (malformed body, blank message_id).
- `go test ./services/gateway/dlq/ -count=1` (unit, no broker) — ok:
  parse table (x-death newest-first, tolerant integer widths via
  topology.RetryCount, absent/garbage x-death, decode_error entries,
  MessageId surviving a broken body), visibility filter, replayDecision
  table (owned→nil, cross-tenant→404, undecodable→409, tenantless→404,
  no-x-death / retry-tier target→409), broker-unavailable typing.
- `TEST_RABBITMQ_URL=amqp://eprocurement:eprocurement@localhost:5672/
  go test ./services/gateway/dlq/ -count=1` — **green twice in a row**
  (~3.4 s each): `TestListSeedsAndFilters` seeds a Suffixed topology
  through the REAL dead-letter path (publish → Get off the main queue →
  Nack without requeue → DLX → DLQ with genuine x-death) and asserts
  depth == planted count (4) although the same call's drain holds them
  unacked, scanned == 4, per-tenant filtering (tenant-a sees 3:
  2 decoded own + 1 decode_error; tenant-b sees its own + the
  decode_error), entry fields (original_queue / reason "rejected" /
  died_at / retry_count from the header), limit≠scan (limit 1, scanned
  4), and the scan cap (scanCap 2 → scanned 2, depth still 4).
  `TestReplayMovesResetsAndRefuses`: cross-tenant replay → 404 and DLQ
  depth unchanged; undecodable replay → 409 and unchanged; unknown id →
  404; owned replay → DLQ 3→2 and the message re-found on its ORIGINAL
  main queue with `x-retry-count` explicitly 0, no `x-death`, body and
  ContentType preserved. `TestBrokerUnavailable` (closed port) → typed
  503 error for both operations. The seeding helper scans the queue for
  the exact message id (a routing key can bind several queues — the FIFO
  head is not necessarily the fresh publish; caught by the first run).
- `go test ./services/worker/dlqmonitor/ -count=1` and with
  `TEST_RABBITMQ_URL=…` — ok both ways: unit (loop ticks immediately +
  on interval, Info at depth 0 / Warn at depth >0, a failing broker read
  is a warn and Run still returns nil on cancel, default interval 1 h)
  and integration (`depth` reads a seeded Suffixed DLQ == 2; `Depth`
  against production names answers error-free; the loop surfaces the
  seeded depth through Sink and stops on cancel).
- `go test ./...` (without TEST_* vars) — the new suites skip cleanly /
  pass, matching the repo convention (CI has no broker service).
- Compose gates: `docker compose config` ok; `docker compose build
  gateway worker` + `docker compose up -d --wait` → all services
  healthy/running (`docker compose ps`); the gateway container carries
  `RABBITMQ_URL=amqp://…@rabbitmq:5672/` (`docker exec … printenv`).
  Note: the pre-existing worker image on this checkout was stale (it
  logged "redis not ready" — code that exists nowhere in the repo);
  after the rebuild the new worker boots with three components
  (`"dlq_summary_interval":"1h0m0s"` in `starting worker`) and its first
  tick logged `{"msg":"dlq depth summary","depth":367}` against the dev
  broker's real DLQ.
- **Live end-to-end smoke** (rebuilt stack, seeded demo admin): login as
  `admin@acme.example` → `GET /api/v1/dlq?limit=3` → 200
  `{"depth":367,"messages":[…3 decoded entries of the caller's tenant
  with event_id/tenant_id/type/occurred_at/retry_count/original_queue/
  reason/died_at…]}`; `POST /api/v1/dlq/replay {"message_id":"nope"}` →
  404 `{"code":"dlq_message_not_found"}`. No replay of a real message
  was performed on the shared dev DLQ (its 367 messages belong to the
  running stack's history; the replay AC is proven by the integration
  suite on isolated topology).
- Broker left clean: `docker exec eprocurement-rabbitmq-1 rabbitmqctl
  list_queues name messages | grep dlqit` — no suffixed test queues.
- pnpm build/lint/test: **not run** — no TypeScript is touched by this
  PR (same honest-reporting convention as dev-notes 046/047); the
  workflow script runs them after this note.

## Follow-ups

- **PR body must carry the two sign-off items** (verification list):
  (a) "DLQ depth metric" realized as live depth in the admin response +
  the worker's hourly structured depth log, Prometheus/OTel export with
  #54; (b) decode_error entries visible cross-tenant (metadata only)
  and answered 409 on replay.
- #54 exports the depth as a real metric (PRD §18) and adds the alerting
  rules formalizing the runbook thresholds.
- A web admin surface (DLQ page on `/admin`) is deliberately out of
  scope; the API contract is stable for it.
- The dev broker's real DLQ (367 messages at verification — `pr.approved`
  events the stack dead-lettered on 2026-10-27, apparently a transient
  Mongo outage) can be replayed or purged with this very tooling once
  this lands.
