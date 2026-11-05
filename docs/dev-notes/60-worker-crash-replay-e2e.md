# Dev note — #60 Playwright resilience e2e: worker crash and replay

- **Issue:** #60 (epic #11, milestone M5, `area:testing`, P1)
- **Branch:** feature/60-worker-crash-replay-e2e
- **Date:** 2026-11-04

> **Provenance:** implemented from the issue plan ("two controlled
> restarts: graceful stop to pile up the outbox, short start to flush to
> RabbitMQ, then a SIGKILL gated on observed queue depth via the
> management API"). The gate mechanics had to be redesigned against
> measured broker behavior — see Decisions; maintainer override welcome in
> PR review.

## What changed

- **`e2e/specs/worker-crash-replay.spec.ts` (new)** — the resilience
  scenario, PRD §22 step 6 / FR-12 / §14.8 as the "one resilience
  scenario" of §23's MVP AC. One sequential test (the #59 convention,
  self-healing preamble + afterAll so a CI retry or a local failure never
  leaves the stack broken):
  1. clean slate (worker running, both projection queues drained, DLQ
     baseline recorded);
  2. the story PR is submitted+approved through the gateway and its
     decision trio is asserted projected (baseline sanity);
  3. **graceful stop** (`docker compose stop worker`) — then 120 approved
     PRs (360 outbox rows) are created through the gateway in 6 parallel
     lanes while NOTHING publishes: the transactional outbox accumulates
     (§14.8 "worker down");
  4. **start** the worker unlocked (boot completes; the consumer appearing
     on `documents.history` proves every boot-time `EnsureIndexes` is
     done), then **stall the projection store** (`db.fsyncLock()` via
     mongosh) — the publisher's 2 s ticks keep flushing the backlog while
     the consumer's `Apply` blocks mid-handler;
  5. **the gate**: poll `rabbitmqctl list_queues` (live) until BOTH
     `documents.history` and `notifications` hold unacknowledged
     deliveries — a stable, re-readable in-flight state (observed live:
     unacked 10 + 10, the full per-subscription prefetch window);
  6. **THE CRASH**: `docker compose kill -s SIGKILL worker`, then
     `db.fsyncUnlock()` immediately;
  7. **AC1 durable proof via the management API**: poll until
     `consumers == 0 && messages_ready > 0` on BOTH queues (observed:
     hist ready 50, notif ready 33 — the orphaned in-flight deliveries
     returned to ready plus the accumulated backlog);
  8. story part 2 **while the worker is dead**: convert → issue →
     acknowledge → full GRN through the gateway (the sync Postgres truth
     moves; PO `received`, items `qty_received == qty_ordered`; the four
     events pile up unpublished in the outbox); the durable backlog is
     re-asserted untouched (§14.8 "queue retains messages");
  9. **restart** (`docker compose start worker`), wait for both queues to
     drain to 0 (120 s budget) and the DLQ to be back at its pre-run
     baseline (nothing new dead-lettered; baseline equality, not absolute
     zero — a dev broker may carry history);
  10. wait for projections to complete, then assert: **AC2** every event
      the run created appears exactly once per entity in the event history
      (the tenant-wide `GET /api/v1/events` walk grouped per entity —
      122 entities, 367 events; the AC's `?entityType=&entityId=` filter
      shape is also exercised directly on the story PO); **AC3** via one
      mongosh pass scoped to the run's entities: `count == distinct
      (event_id)` for `events`, `notifications`, `audit_log`, exact
      expected totals, per-type notification parity (one notification per
      `pr.submitted`/`pr.approved`/`po.issued`/`po.acknowledged` event),
      and the notification type set exactly the routed subset; **AC4**
      exactly one `grn.posted` event and one audit `post` row for the PO,
      API items `qty_received == qty_ordered == 2`; **AC5** PO `received`
      via `GET /api/v1/purchase-orders/{id}` and in the UI (PO detail
      page: status badge, Received == Ordered, Remaining 0, no receipt
      form).
- **`e2e/support/rabbitmq.ts` (new)** — the management-API client
  (`queueStats`, `waitForQueue`, `waitForAllQueues`) plus the topology's
  queue-name constants. The management API is the AC's witness for STABLE
  states; it aggregates queue stats coarsely, so it is never used for
  sub-second gating (see Decisions).
- **`e2e/support/stack.ts` (new)** — compose lifecycle controls
  (`stopWorkerGracefully`, `killWorker` = `docker compose kill -s SIGKILL
  worker`, `startWorker`, `ensureWorkerRunning`), `brokerQueueDepths()`
  (live `rabbitmqctl list_queues` inside the compose rabbitmq container —
  the broker's own view, no management-DB lag), `mongoEval()` (mongosh in
  the compose mongo container, the assertions' channel), and
  `mongoFsyncLock`/`mongoFsyncUnlock` (the crash gate's stall — see
  Decisions).
- **`e2e/support/gateway.ts`** — flow helpers the resilience story drives
  over REST: `createRequisition`/`submitRequisition`/`decideApproval`,
  `issuePurchaseOrder`/`acknowledgePurchaseOrder`/`postGoodsReceipt`,
  `getPurchaseOrder`, and `listEvents`/`listEntityEvents` (keyset-paged
  `GET /api/v1/events`, tenant-wide or per-entity — documents requires
  both-or-neither of `entityType`/`entityId`).
- **`e2e/support/fixtures.ts`** — tenant C (`66666666-…`), the spec's own
  tenant: two fixture users (requester + second same-tenant approver —
  self-approval is forbidden for everyone, admins included), one vendor,
  and the RabbitMQ-management / Mongo endpoint constants (env-overridable,
  compose's public local-dev defaults as fallbacks). A dedicated tenant
  keeps the exactly-once assertions exact over this run's entities even
  when the happy-path spec or a CI retry ran before.
- **`e2e/support/fixtures.global-setup.ts`** — installs tenant C's two
  users (idempotent `cmd/useradmin` upsert), vendor (409-tolerant), and
  the single-step IDR approval chain, the #59 onboarding-§5.1 pattern.
  (One file beyond the issue plan's expected list — the established
  fixture-install point; smallest diff vs duplicating the useradmin exec
  in the spec.)
- **`Makefile`** — the `e2e` target's comment/help now names both specs
  and the docker/mongosh prerequisites. Recipe unchanged.
- **`README.md`** — status line, repo tree, make-target table, the
  capability matrix's Playwright row (the "#60 pending" marker is
  replaced by the implemented scenario), the demo stand-in note, and the
  CI E2E bullet.
- **Not changed:** `compose.yaml`, `.github/workflows/ci.yml` (the e2e
  job already runs `pnpm --filter @eprocurement/e2e e2e`, which now picks
  up both specs), any Go service, `apps/web`, `pnpm-lock.yaml` (no new
  dependencies).

## Decisions

- **The crash gate stalls the projection store (fsyncLock), because a
  bare observation-gated SIGKILL cannot be made deterministic on this
  stack.** Measured on the dev compose stack (all numbers from throwaway
  scripts run during implementation):
  - the worker is ONE binary: its consumer settles each delivery ~2–5 ms
    after the publisher publishes it (worker logs show publish/settle
    interleaved per message) — a healthy queue never accumulates depth;
    a 50-row publisher burst (the batch cap) is fully drained in
    ~100–150 ms, so the in-flight window (unacknowledged deliveries) is
    ~7 % duty per 2 s tick;
  - the management HTTP API aggregates queue stats coarsely (a purge was
    still reported as 367 messages a minute later), so sub-second gating
    through it is impossible — `rabbitmqctl list_queues` inside the
    broker container is the live view;
  - `docker pause` is fast (66–91 ms) and reversible, and a phase-locked
    freeze aimed at the publisher tick (period measured 2003 ms) still
    landed idle in 8/8 sweep attempts — the docker CLI's latency vs the
    ~100 ms window is a coin flip per attempt;
  - stalling mongod (`db.fsyncLock()`, the backup write lock — §14.8's
    "Mongo down" row realized as a stalled projection store, without the
    crash) blocks the consumer's `Apply` mid-handler: its unacknowledged
    deliveries (the per-subscription prefetch caps out at 10+10 across
    the two queues) and the ready depth the publisher keeps adding become
    a STABLE state the gate reads via rabbitmqctl, and the SIGKILL cannot
    miss it. The lock is released immediately after the kill (and
    best-effort in `afterAll`).
  - order matters: the stall is taken only AFTER the consumer attaches
    (`consumers > 0`) — a lock taken before the start hangs the worker's
    own boot `EnsureIndexes` on mongod (verified: consumers never
    attached within 40 s; the stall then only ever pins the consume path.
- **The kill itself is exactly the AC's mechanism**: `docker compose kill
  -s SIGKILL worker`, issued once the gate has observed both queues
  holding in-flight deliveries. The stall is auxiliary scaffolding to
  make the mid-flight state observable; the SIGKILL orphans the pinned
  delivery (at-least-once redelivery) and any outbox rows the publisher
  had not yet marked are republished after the restart — both dedupe
  paths are then proven by the exactly-once asserts.
- **API-driven story, UI at the ends.** The happy-path spec owns the UI
  journey; the resilience spec drives the flow over the gateway (fast,
  robust — 120 backlog PRs in ~1 s via parallel lanes) and uses the UI
  where the AC names it (the PO detail page's `received` state). The
  convert step keeps the #59 maintainer decision (direct gateway call; no
  convert UI, #112).
- **Own tenant (C) + this-run entity scoping.** Every assertion is scoped
  to the entity ids created by THIS run (story PR, backlog PRs, PO), so
  re-runs and CI retries stay exact on a dirty tenant; the tenant-wide
  `GET /api/v1/events` walk is grouped client-side (N entities in ~2–3
  paged calls instead of N calls).
- **DLQ assert is baseline equality, not zero.** A fresh CI broker starts
  at 0, but a dev broker may carry history (mine held 367 messages from
  the implementation's kill/pause experiments); the property under test
  is "the replay dead-lettered nothing", so depth is read live
  (rabbitmqctl) before and after and asserted equal.
- **All fixture logins fit identity's local 5/min-per-email limiter in a
  single attempt** (tenant C admin: setup + API + UI = 3; approver:
  setup + API = 2); CI raises the limiter to 60 for retries (the #59
  convention, same env knob).
- **`tsc --noEmit` is the e2e package's compile gate** (it ships no
  `build`/`test` scripts — Playwright never runs without a stack; #59's
  convention), and `pnpm lint` covers it via its eslint config.

## Evidence

All commands from the repo root against the live compose stack
(PostgreSQL on the local override `:55432`, MongoDB `:27017`, RabbitMQ
`:5672` + management `:15672`, gateway `:8080`; every service healthy per
`docker compose ps`); `DATABASE_URL` passed for the suite's globalSetup
(the `make e2e` pattern):

- `pnpm --filter @eprocurement/e2e exec tsc --noEmit` — clean.
- `pnpm --filter @eprocurement/e2e lint` — clean.
- `DATABASE_URL=… pnpm --filter @eprocurement/e2e e2e --grep "worker
  crash and replay"` — **passed twice back-to-back** (35.0 s, then 33.8 s
  on the dirty tenant — the repeatability check). Reported evidence line
  (run 1): crash gate in-flight at kill `hist{ready:40,unacked:10}
  notif{ready:23,unacked:10}`; durable after kill `hist{ready:50}
  notif{ready:33}`; totals `entities=122 events=367 notifications=244
  audit=244` — every event exactly once per entity, no duplicate
  projections, one notification per routed event, one `grn.posted` + one
  audit `post` row, PO `received` in API and UI.
- `make e2e` — **2 passed (48.4 s)**: happy path (3.3 s) then the
  resilience spec (40.4 s) in one suite, exactly what the CI e2e job
  runs.
- `go vet ./... && go build ./...` — clean. `go test ./...` — 40 packages
  ok (no Go files touched).
- `pnpm install --frozen-lockfile` — clean; `pnpm build` — 3/3; `pnpm
  lint` — 4/4 (incl. `@eprocurement/e2e`); `pnpm test` — 6/6.
- Gate-mechanics measurements (throwaway scripts, since deleted):
  consumer keeps pace with the publisher (depth never > 0 at 100 ms
  sampling over a 45 s fed flow); management API stats coarsely
  aggregated; `docker pause` 91 ms / unpause 62 ms; publisher tick period
  2003 ms; phase-locked freezes 0/5 offsets caught in-flight; fsyncLock
  before start hangs boot (0 consumers in 40 s) — the shipped order
  (start → consumers attached → lock) pins within ~2 s and the gate reads
  unacked 10+10.
- **CI (`e2e` job) — not run from here**: no push/PR happens before
  review (the workflow script commits after). The job is unchanged and
  already executes `pnpm --filter @eprocurement/e2e e2e`, which runs both
  specs; the PR body must link the first green run. Local prerequisites
  match the job's runner (docker compose CLI + the compose stack + a
  Node 22 pnpm workspace).

## Follow-ups

- **First CI execution of the two-spec e2e job** — link the run in the PR
  body once pushed (the #59 convention).
- **README "Demo & review artifacts" section** still lists the demo
  script (#63) as open although it landed (pre-existing staleness noticed
  while editing the status line; left alone here — one issue per PR).
- The crash gate's scaffolding (fsyncLock) is a pragmatic determinism
  lever; if the maintainer prefers a "pure" kill (no foreign-system
  stall), the alternatives are a `WORKER_POLL_INTERVAL`/`WORKER_BATCH_
  SIZE` compose passthrough (an infra change) or accepting retry-based
  flakiness (~25 % hit per attempt measured). Flagged for PR review.
- The events tenant-wide walk and the mongosh assertions scale with the
  run's entity count (122 entities ≈ 3 s); if the backlog ever grows
  much larger, consider asserting a sampled subset plus the collection
  totals (the mongosh pass already covers totals exactly).
- Local login-rate note (same as #59): back-to-back full-suite runs
  within a minute can trip identity's 5/min per-email limiter on the
  fixture users; space runs or set `IDENTITY_LOGIN_RATE_PER_MIN` in
  `.env` (CI already sets 60).
