# Dev note — #58 Integration tests for procure-to-pay flows

- **Issue:** #58 (part of #11, milestone M4)
- **Branch:** feature/58-integration-tests-procure-to-pay
- **Date:** 2026-11-01

## What changed

- **`tests/integration/` (new, test-only package)** — the procure-to-pay
  cross-service suite ADR-0002's stage 3 calls for: one story driven through
  the REAL procurement gRPC API in-process, whose outbox rows are drained by
  the REAL worker publisher `Loop` over RabbitMQ into the REAL consumer
  `Runner`, projecting into REAL MongoDB. The HTTP edge (gateway/web) is the
  only hop excluded — #59's Playwright suite owns it.
  - **`harness_test.go`** — per-RUN fixtures: a scratch PostgreSQL database
    (`DROP`+`CREATE`+`dbmigrate.Apply` of the same embedded migrations
    `cmd/migrate` applies; the maintenance-db pool drops it again at
    teardown), a scratch MongoDB database (dropped at teardown, indexes
    ensured through the `cmd/worker` boot path BEFORE consumption), and a
    suffixed copy of the production Rabbit topology
    (`topology.Default.Suffixed`, 150 ms retry TTLs, `Delete` at teardown).
    The scratch Postgres database is what neutralizes the race with the
    live compose worker: the running worker's `outbox.Pending` has no
    `SKIP LOCKED`, but it polls the compose database, never this one.
    Per-TEST fixtures: fresh tenant/user UUIDs and a fresh gRPC server, plus
    FK-safe child→parent cleanup over all 17 procurement tables the flow
    writes (item tables by parent subselect — they carry no tenant_id per
    PRD §13; `outbox` deleted explicitly — it has no FK), before AND after
    every test, via `tenant.WithBypass`.
  - **`p2p_flow_test.go`** — the happy path: install the one-step IDR
    approval chain → vendor → PR(items) → submit → self-approval rejected
    (`approval_self_forbidden`, admin notwithstanding, step stays pending) →
    approve as the second user → convert → issue → acknowledge → full GRN
    (PO `received`) → invoice → match `matched`. The conversion idempotency
    is asserted IN the flow: replaying `CreatePurchaseOrder` with a
    different vendor id returns the SAME PO (`created=false`, identical
    id/`created_at`, vendor gate skipped), still exactly one PO row and one
    `po.created` outbox row.
  - **`p2p_failure_test.go`** — the six failure paths, each on its own
    tenant, each asserting the typed gRPC code + machine message AND state
    consistency (rows unchanged, outbox count unchanged, no projected
    event): GRN over-receipt (`Aborted` `grn_over_receipt: …` detail line),
    draft-PR conversion (`Aborted` `po_requisition_not_approved` — the
    pinned gate, replacing the plan's "second conversion fails" slot which
    the idempotent store makes impossible), self-approval, cross-tenant
    get/decide/convert (all `NotFound`, zero information leaked, tenant A
    untouched), cancelled-PO receipt (`Aborted` `po_illegal_transition`),
    and invoice discrepancy (partial receipt + full invoice → status
    `discrepancy`, `invoice.discrepancy` event which IS an audit row).
- **`services/worker/publisher/publisher.go`** — `ConnectTopology(ctx, url,
  confirmTimeout, topo)`: the topology-aware constructor. The zero Topology
  resolves to `topology.Default` (the `consumer.New` convention), `Declare`
  replaces connect's bootstrap declarations, and `Publish` targets the
  topology's events exchange. The production `Connect` is byte-for-byte in
  behavior: same bootstrap declarations, `p.exchange` pinned to
  `EventsExchange`; the only shared line is `publishConfirmed` now
  publishing to `p.exchange` (identical value for `Connect`). The
  reconnect path re-declares the variant's own topology (`p.topo`), so a
  broker restart mid-test restores the suffixed copy. The
  `cmd/worker/main.go` call site is untouched.
- **`services/worker/publisher/publisher_test.go`** — `TestResolveTopology`
  (pure unit: zero → Default, suffixed passes through) and
  `TestConnectTopologyDeclaresSuffixedTopology` (broker integration,
  skipping without `TEST_RABBITMQ_URL`: the variant's FULL queue set exists
  after connect and a publish routes through the suffixed exchange). This
  is honestly an integration test, not a hermetic unit — `Connect` dials at
  construction and amqp091-go's `Channel` is a concrete type, so the
  declaration path needs a socket; the pure resolution logic is the part
  unit-tested. The production-`Connect` regression stays
  `TestAMQPPublisherEndToEnd`'s passive checks, re-run green in Evidence.
- **`Makefile`** — `make test-p2p`: runs `go test ./tests/integration/...
  -count=1 -v` with `TEST_DATABASE_URL`/`TEST_MONGO_URI` read from `.env`
  (the existing sed pattern) plus the NEW `TEST_RABBITMQ_URL ?=` passthrough
  (`.env`'s `RABBITMQ_URL`, `.env.example:47`). Scoped to the suite only,
  deliberately NOT `./...`: with `TEST_RABBITMQ_URL` exported and the full
  stack running, the older broker tests that share the compose database
  (the publisher's scratch-database loop test) would race the live worker —
  the exact race the new suite's scratch database exists to avoid.
- **`.github/workflows/ci.yml`** — new `integration` job (ADR-0002 stage 3,
  `needs: [go]`, 15 min): starts ONLY the compose infrastructure
  (`docker compose up -d --wait postgres mongo rabbitmq`) and runs
  `make test-p2p` — the same sequence as local, no service images built.
  The `go`/`web`/`e2e` jobs are unchanged.
- **`docs/onboarding.md`** — the testing paragraph now names the third
  tier: `make test-p2p` against the live compose infrastructure.

## Decisions

- **Event asserts are tie-aware, ordered, and tenant-filtered (AC fix).**
  The suite reads the tenant's events sorted by `(occurred_at, event_id)`
  (the `history.List` order, ascending) and matches the expected sequence
  with tie groups: events sharing an `occurred_at` (Mongo millisecond
  precision) are unordered among themselves, distinct timestamps strict.
  The approval engine writes `pr.step_approved` + the terminal event with
  ONE `DecidedAt` (`appendDecisionEventsTx`), so a bare occurred_at sort
  orders that pair arbitrarily — a strict assert would flake ~50%. The
  happy path asserts the EXACT eight-event set (nothing extra): the
  sequence is `pr.submitted, pr.step_approved, pr.approved, po.created,
  po.issued, po.acknowledged, grn.posted, invoice.matched`; the audit
  assert pins the closed `AuditAction` set — exactly five rows
  (`approve, approve, issue, post, match`), with `pr.submitted` and
  `po.acknowledged` producing none. `invoice.matched` audits the INVOICE
  aggregate (`match/store.go`), the other decisions their own entities.
- **Isolation = unique tenant per test, NOT Mongo drops per test (review
  constraint).** Go runs a package's test files in alphabetical order
  (`p2p_failure_test.go` before `p2p_flow_test.go`), so with a fixed tenant
  the happy path would inherit the failure tests' events (`po.cancelled`,
  `invoice.discrepancy`) in the shared scratch Mongo database. Fresh
  tenant/user UUIDs per test make every Mongo assert exact under its
  `tenant_id` filter and every Postgres cleanup a by-tenant delete; the
  before-AND-after cleanup stays as a crash-recovery guard.
- **The publisher's `po_item` ids come from the acknowledge response.** The
  convert response echoes the copied basket WITHOUT the DB-generated ids
  (`insertPO` returns the aggregate's items); the lifecycle loads carry
  them (`LoadForUpdate` → `loadItems`). First observed here as
  `grn_po_item_unknown`; documented in the test, no production change.
- **`DROP DATABASE` is deadline-bounded and leak-tolerant.** Mid-verification
  the suite hung after teardown: FIVE stuck `DROP DATABASE` backends, all
  waiting on `ProcSignalBarrier`, because ONE wedged backend elsewhere on
  the server (a 3-hour-active `INSERT INTO procurement.invoices` whose
  client died — it processed neither `pg_cancel_backend` nor
  `pg_terminate_backend`) blocks the system-wide barrier. A pure-psql probe
  (`CREATE DATABASE` + `DROP DATABASE`) reproduced the hang with no Go code
  involved. Teardown now tries plain `DROP` then `WITH (FORCE)`, each with a
  5 s context, and on timeout deliberately leaks the nanos-named scratch
  database with one stderr line — a wedged environment can slow the suite
  by 10 s but never hang it. The wedged backend itself is an environment
  artifact predating this session (follow-up: restart the compose postgres).
- **`make test-p2p` scope and the CI job's shape.** Local: the three
  `TEST_*` vars come from `.env` so a port override (e.g. 55432) works
  unchanged. CI: `?=` in the Makefile keeps the job's exported `TEST_*`
  (compose's no-`.env` defaults on the runner), so `make test-p2p` runs the
  identical sequence in both places; the job starts only the three infra
  services — the suite boots the gRPC server and both worker halves itself.
- **`NewLoop` interval is a constructor parameter** — the harness passes
  50 ms explicitly; the 2 s/50 values are only `main.go`'s pinned config.
  Asserts poll at 50 ms with a 15 s hard deadline (the
  `consumer_integration_test.go` convention), and each step's
  mutation→projection latency is recorded and logged as FR-11 evidence —
  deliberately NOT a hard gate, so a loaded CI runner cannot flake the
  suite against the 2 s budget.

## Evidence

All commands from the repo root against the live compose stack (PostgreSQL
16 on the local override `:55432`, MongoDB `:27017`, RabbitMQ `:5672`, all
healthy per `docker compose ps`, the compose worker running — the scratch
database isolation is therefore exercised for real):

- `make test-p2p` — **7/7 PASS in 12.5 s** (happy path 0.54 s), twice in a
  row back-to-back (12.6 s / 12.5 s) for repeatability. Per-event
  mutation→projection latencies (FR-11 budget: within 2 s, PRD line 179),
  second run: submit 63 ms, approve 112 ms, convert 60 ms, issue 58 ms,
  acknowledge 61 ms, grn 59 ms, match 58 ms — every step two orders of
  magnitude inside the budget on the dev stack.
- `go test ./tests/integration/ -count=1` (no `TEST_*` set) — skips cleanly,
  `ok` without stack: plain `go test ./...` stays green without
  infrastructure.
- `go vet ./... && go build ./...` — clean with the new test-only package.
- `TEST_DATABASE_URL=… TEST_RABBITMQ_URL=… go test ./services/worker/publisher/
  -run 'TestResolveTopology|TestConnectTopologyDeclaresSuffixedTopology|
  TestConnectTopologyCancelledContext|TestConnectFailure|TestEnvelopeFromRow'
  -count=1` — green (0.43 s): the pure topology-resolution table, the
  broker-side declaration + routing proof of `ConnectTopology` (full
  suffixed queue set + capture-queue roundtrip), the cancelled-context
  failure path, and the untouched existing tests.
- `TEST_DATABASE_URL=… TEST_RABBITMQ_URL=… go test ./services/worker/publisher/
  -run TestAMQPPublisherEndToEnd -count=1` — green (0.12 s): the production
  `Connect` regression (bootstrap exchange/DLX/DLQ + confirm-mode publish
  + marked row) after the `p.exchange` refactor.
- `TEST_DATABASE_URL=… TEST_MONGO_URI=… TEST_RABBITMQ_URL=… go test
  ./services/worker/consumer/ ./services/worker/topology/ -count=1` — green
  (4.4 s / 2.9 s): the consumer and topology packages unaffected.
- `docker compose config -q` — valid (no compose.yaml change needed; the CI
  job rides the existing infra services).
- `python -c "yaml.safe_load(.github/workflows/ci.yml)"` — jobs
  `[go, web, integration, e2e]`, `integration.needs == [go]`,
  `timeout-minutes == 15`; the go/web/e2e jobs are untouched.
- **Pre-existing, environment-caused (not this diff):** with the wedged
  backend described in Decisions still on the dev box,
  `go test ./services/worker/publisher/ -run
  TestLoopRunAgainstUnmigratedDatabase` hangs inside `scratchDatabase()`'s
  `DROP DATABASE IF EXISTS eproc_test_45_scratch` (the same barrier wait a
  pure-psql `DROP DATABASE` hits); the test and helper are untouched by
  this PR and pass on a healthy server (CI runs the publisher package
  without `TEST_DATABASE_URL`'s scratch path gating — it skips). Full
  `go test ./...` and `pnpm` gates are re-run by the workflow script after
  this note.
- CI's `integration` job runs for the first time on this branch's PR push;
  its result is the remaining acceptance evidence (mirrors the verified
  local `make test-p2p` on a fresh runner).

## Follow-ups

- **Restart the compose postgres** to clear the wedged backend (pid observed
  2026-11-01; it ignored cancel/terminate) and drop the leaked
  `eproc_p2p_it_*` scratch databases plus the wedged 3-hour INSERT's
  transaction — maintainer action on the dev stack, not done from here.
- **First CI execution** of the `integration` job lands with this PR's
  push — link its run in the PR body once green.
- The convert response's item basket carries no DB ids (see Decisions); if
  a consumer ever needs them in-band, that is a small procurement API
  change, not a test concern.
- The older DB-shared broker tests (`publisher` scratch-database loop test)
  still assume no live worker against their shared database;
  `make test-integration` keeps excluding `TEST_RABBITMQ_URL` — revisiting
  them for scratch-database isolation is a separate chore if the stack-up
  test matrix grows.
