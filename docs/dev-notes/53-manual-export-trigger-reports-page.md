# Dev note — #53 Manual export trigger on the reports page

- **Issue:** #53
- **Branch:** feature/53-manual-export-trigger-reports-page
- **Date:** 2026-11-02

> **Provenance:** the three parked questions were answered on issue #53
> (comments — the agent-applied decision record, maintainer override
> welcome in PR review):
>
> 1. **Only the manual trigger is admin-only** — a new `export:trigger`
>    permission with an EMPTY RBAC matrix entry (the exact `dlq:manage`
>    twin from #48); #49's `export:run` read access (finance/admin) is
>    deliberately NOT demoted.
> 2. **Manual runs use a separate object key**
>    `exports/{tenant}/{yyyy-mm-dd}/purchase-orders-manual-{HHMMSS}.csv`
>    (date AND clock in `EXPORT_TZ`) — the dated nightly artifact #49
>    pinned is never overwritten.
> 3. **`export.completed`/`export.failed` stay unemitted** — #49's
>    documented gap, kept: the worker must not write the procurement
>    outbox, and publishing straight to the exchange would bypass the
>    transactional-outbox discipline; the Mongo run history is the record.

## What changed

- **`internal/platform/rbac` + `docs/rbac.md`** — permission
  **`export:trigger`** (matrix entry `nil` — admin-only, the
  `dlq:manage`/`match:configure` pattern) with the mandatory
  `wantPermission` mirror and the docs table row + landed note.
  `export:run` (finance/admin) untouched.
- **procurement** — RPC **`TriggerExport`** (proto + gen via
  `make gen-proto`; `TriggerExportRequest{}` /
  `TriggerExportResponse{event_id}`), mapped in `methodPermissions` to
  `rbac.ExportTrigger`. The handler is one call into the new thin store
  `services/procurement/export` (`Store.Request`): inside ONE
  `tenant.WithTenant` transaction it appends exactly one outbox row —
  `EventType "export.requested"` (§14.6), `EntityType "export"`,
  `EntityID` = the generated event id (the request IS the aggregate),
  actor/ip/trace from the gateway-propagated context (the purchaseorder
  store's AppendTx pattern). No other state changes; RLS enforces the
  tenant (outbox is FORCE RLS, migration 0006). `cmd/procurement` wires
  the store into `server.New`.
- **documents** — `export.Run` gains **`Trigger`**
  (`TriggerScheduled = "scheduled"` / `TriggerManual = "manual"`)
  additively through `RunBSON`/`runDoc`/`toRun`; the nightly scheduler now
  SETS `Trigger: TriggerScheduled` on every new record (enum filled on
  both paths); proto `ExportRun` gains `trigger = 10` (regen) and
  `runPB`/the gateway's `exportRunJSON` carry it. The #49 detection
  semantics are preserved by scoping `Latest`, `HasSuccess`, and
  `MarkNewestMissedPrevious` to scheduled runs via `trigger: {$ne:
  "manual"}` — `$ne` keeps pre-#53 documents (no field) in scope as
  scheduled. `List`/`Get` serve both. The private scheduler constant
  `recordWriteBudget` is now the exported **`RecordWriteBudget`** (the
  manual runner's timeout arithmetic needs it).
- **worker** — **`services/worker/export/manual.go`** (new):
  `ManualRunner` implements `consumer.Handler`. Per delivery: the export
  window is the trailing 24 h **anchored on the event's `occurred_at`**
  (deterministic across broker retries), the CSV lands under the manual
  key (clock segment `HHMMSS` in `EXPORT_TZ`), and the record is upserted
  with `trigger="manual"`, `scheduled_for=occurred_at` — the
  `{tenant_id, scheduled_for}` upsert IS the retry idempotency. The export
  runs under `WithTimeout(ctx, EXPORT_JOB_BUDGET)`; the record is written
  on `WithTimeout(WithoutCancel(ctx), RecordWriteBudget)` — the runOwned
  pattern verbatim, so a budget-exceeded or SIGTERM-cancelled run still
  records `status=failed` + `error_message`. A failure ALSO returns the
  error, so the broker ladder (5 s/30 s/5 m → DLQ) applies. No Redis lock,
  no heartbeat, no missed-run stamping. `ManualDeliveryTimeout(budget)`
  computes the runner cap (budget + `RecordWriteBudget` + 30 s margin) and
  errors on a non-positive budget. A stray non-`export.requested` event is
  an error (climbs the ladder to a visible DLQ copy), never a silent ack.
- **worker wiring** (`cmd/worker/main.go`) — a **second consumer.Runner**
  dedicated to `topology.ExportsQueue` (handler = ManualRunner,
  `Prefetch: 1`, `DeliveryTimeout = ManualDeliveryTimeout(cfg.
  ExportJobBudget)`), registered as the component `exports-consumer`.
  The #47 runner (history/notifications) keeps its default 10 s
  DeliveryTimeout — its hang-queue guarantee is unchanged. Both runners
  declare the same idempotent topology; `consumer.New` accepts a handler
  subset (its comment anticipated exactly this).
- **gateway** — `POST /api/v1/exports` behind
  `RequirePermission(rbac.ExportTrigger)` in `exports.go`, answering
  **202 `{"status":"requested","event_id":…}`**. `ProcurementClient`
  gains `TriggerExport`. `registerExportRoutes` widened to
  `(app, verifier, documents, procurement)` with explicit nil-guards:
  GET routes register iff `documents != nil`, POST iff
  `procurement != nil` — no path dereferences a nil interface; the call
  moved out of server.go's documents-only block so procurement-only apps
  still expose the trigger. `exportRunJSON` surfaces `trigger`.
- **web** — `triggerExport` in `lib/gateway.ts` (+ `ExportRequested`
  type, `trigger` on `ExportRunEntry`); BFF `POST /api/exports`
  (`apps/web/src/app/api/exports/route.ts`, the runs-route pattern,
  202 mirror); `ExportRunsTable` renders the admin-only "Run export now"
  button (`canTrigger` from `session.claims.roles`, the SidebarNav admin
  gating pattern), invalidates `["export-runs"]` on success so the manual
  row appears, shows the `manual` badge on `trigger === "manual"` rows,
  and keeps the existing download/error surfaces. i18n: `reportsTable`
  extended (trigger/triggering/triggerFailed/manualBadge), both locales.

## Decisions

- **The trigger is one outbox row, nothing else.** There is no business
  state to change with a manual export request, so the transactional
  unit IS the event (ADR-0001's discipline — no dual writes, and the
  worker that runs the export must never write procurement's outbox).
  202 Accepted is the honest status: the work is queued, and `event_id`
  correlates the request with the queued message.
- **A dedicated runner, not a bigger #47 runner.** Raising the shared
  runner's DeliveryTimeout would weaken the history/notifications
  hang-queue guarantee (dev-note 047). The exports queue gets its own
  runner with a cap derived from the same env knobs as the scheduler —
  `budget + RecordWriteBudget + 30 s` — with a fail-fast constructor
  error when the budget is non-positive (the NewScheduler convention).
  Prefetch 1: one long job at a time; buffering helps nothing.
- **Detection stays scheduled-only.** Without the `$ne` scope, a manual
  success would satisfy `HasSuccess` for the nightly slot (silently
  resetting the 26 h clock's evidence) and a newer manual record would
  steal `Latest`/the boot flag. `List`/`Get` are unfiltered — the history
  shows both. Pre-#53 documents have no `trigger` field; `$ne: "manual"`
  keeps them scheduled-side, so nothing about #49's behavior changes.
- **Window anchored on `occurred_at`, not consume time.** A message that
  sits in the queue (or climbs the retry ladder) must export the same
  window on every attempt; `occurred_at` is the immutable anchor.
- **Failure writes the record AND returns the error.** The record is the
  operator surface (#49's error display); the returned error is what
  engages the broker ladder so a transient PG/MinIO outage self-heals
  and a permanent one lands in the DLQ — visible, replayable (#48).
- **`export.completed`/`export.failed` still not emitted** (decision 3):
  emitting them would need either the worker writing procurement's
  outbox (forbidden) or a direct exchange publish (bypasses the outbox
  discipline, ADR-0001). The deviation is documented here and in
  docs/schema/documents.md.
- **Known edge, explicitly accepted (decision 2):** two manual triggers
  within the same second share the `HHMMSS` key segment — the later
  write wins on the object while both run records exist, so the earlier
  run's presigned download serves the later run's bytes. Documented in
  docs/schema/documents.md rather than silently absorbed; sub-second
  key segments were rejected (S3/MinIO key churn, no operator value).

## Evidence

All commands run from the repo root against the local compose stack
(PostgreSQL :55432, MongoDB :27017, RabbitMQ :5672):

- `go test ./internal/platform/rbac/ -count=1` — ok (the `ExportTrigger`
  mirror passes `TestMatrixCoversEveryPermission` /
  `TestEveryRoleAgainstMatrix`).
- `TEST_DATABASE_URL=postgres://eprocurement:eprocurement@localhost:55432/
  eprocurement?sslmode=disable go test ./services/procurement/server/
  -count=1 -run TestTriggerExportGRPC -v` — PASS: admin triggers →
  exactly one `export.requested` outbox row (tenant, actor, entity
  export) + `event_id` returned; finance AND buyer →
  `PermissionDenied` naming `export:trigger`; anonymous →
  `Unauthenticated`; denied calls write nothing.
- `TEST_MONGO_URI=mongodb://eprocurement:eprocurement@localhost:27017/
  ?authSource=admin go test ./services/documents/export/ -count=1` — ok,
  including the new `TestManualRunsInvisibleToDetection`: Latest/HasSuccess/
  boot-flagging scope to scheduled runs while List/Get serve the manual
  record with its trigger; `$ne` keeps field-less pre-#53 rows scheduled.
- `go test ./services/worker/export/ -count=1` — ok, 10 new tests:
  manual object key table (UTC + Asia/Jakarta date rollover, a-minute-
  apart keys distinct), constructor validation (zero/negative budget,
  bad zone), happy path (window = trailing 24 h before occurred_at, CSV
  under the manual key, record trigger=manual/scheduled_for=occurred_at),
  retry overwrite (same upsert key), failure records + returns the error,
  budget-exceeded still records, parent-cancel still records
  (WithoutCancel), stray event type errors without work,
  `TestManualDeliveryTimeout` (budget+record+margin, strictly above the
  budget and above the #47 default 10 s), and
  `TestManualRunnerSucceedsUnderRunnerCap` — the runner-cap proof: under
  the wired cap a 150 ms job with a 300 ms budget succeeds; under a
  50 ms cap it fails. Plus the nightly `Trigger: scheduled` assertion in
  `TestRunTenantSuccess`.
- `go test ./services/gateway/server/ -count=1` — ok, including the new
  `TestTriggerExportRoute` (admin 202 `{status: requested, event_id}`,
  tenant metadata propagated, Unavailable → 502 through the shared map),
  `TestTriggerExportRouteRequiresPermission` (finance AND buyer → 403
  `permission_denied` + `required: export:trigger`, no procurement call;
  anonymous → 401), `TestExportRoutesWithoutProcurement` (GET works, POST
  404 — no panic on the nil interface), and
  `TestExportRoutesWithoutDocuments` (trigger 202, GET 404).
- `pnpm vitest run "src/app/(app)/app/reports/ExportRunsTable.test.tsx"
  "src/app/api/exports"` (apps/web) — 24 passed across 4 files: the new
  BFF POST route (401 without cookie and no gateway call, bearer
  forwarding as POST, 202 envelope mirror, 401/403 ride-through, 502 on
  network) and the table's new cases (manual badge on manual rows only,
  trigger button renders only for canTrigger, click → POST /api/exports →
  invalidate → manual row appears, trigger failure state).
- `pnpm lint` (apps/web) — clean. `go vet ./... && go build ./...` —
  clean (run during implementation; the full gate set is re-run by the
  workflow after this note, per the ask).
- `make gen-proto` — only the two expected gen pairs + protos changed
  (`git status`).
- **Live end-to-end against the rebuilt compose stack** (gateway,
  procurement, worker, documents rebuilt; `make seed` restored the demo
  data after finding the shared Mongo `events` projection pre-damaged —
  indexes dropped, 68 stray documents with duplicate `event_id`s —
  residue predating this branch; the documents tests use per-run scratch
  databases, `services/documents/internal/testenv`):
  - worker boot log: `"export_queues":["exports"],
    "export_delivery_timeout":"10m40s"` — the second runner is live
    (10 m budget + 10 s record + 30 s margin).
  - `POST /api/v1/auth/login` (admin@acme.example) →
    `POST /api/v1/exports` → **202
    `{"event_id":"f9fab3fb-…","status":"requested"}`**.
  - seconds later the worker logged `manual export run succeeded …
    tenant_id 11111111-…-1111 … rows 1, object_key exports/11111111-…/
    2026-11-01/purchase-orders-manual-215328.csv` (the HHMMSS = the
    event's occurred_at in EXPORT_TZ), and `GET /api/v1/exports/runs`
    shows the record first: `"trigger":"manual"`, `"status":"success"`,
    `scheduled_for = 2026-11-01T21:53:28Z` (= occurred_at).
  - `GET …/runs/{id}/download-url` → presigned GET; `curl` of it
    streamed the exact pinned CSV (`po_number,vendor_code,vendor_name,
    currency,total_cents,status,issued_at` header +
    `PO-000002,ACME-01,Acme Supplies 01,IDR,300000,issued,
    2026-11-01T03:15:38Z`); MinIO holds the manual object next to the
    untouched nightly ones.
  - Denials live: buyer POST → 403 `{"code":"permission_denied",
    "required":"export:trigger"}`; buyer GET runs → 403 (`export:run`
    surface unchanged); anonymous POST → 401 `missing bearer token`.
    (The demo tenants seed no finance user; the finance-denied case —
    finance holds `export:run` but NOT `export:trigger` — is pinned by
    `TestTriggerExportGRPC` and `TestTriggerExportRouteRequiresPermission`
    at both layers.)
  - `rabbitmqctl list_queues`: `exports` and all three retry tiers at 0 —
    fully drained. Bonus proof of the outbox path: the new worker also
    consumed the `export.requested` row `TestTriggerExportGRPC` had left
    in the shared dev DB (tenant `dddddddd-…-0001`, its own manual CSV) —
    procurement outbox → publisher → exports queue → runner → MinIO,
    one flow.

## Follow-ups
- The same-second object-key collision (the known edge) could be closed
  later by appending a short hash/uuid suffix to the manual key — a
  maintainer call, deliberately not done (decision 2 pinned the key
  shape).
- `export.completed`/`export.failed` remain a conscious PRD §14.6 gap
  (decision 3); if they ever land, procurement must own the writes (e.g.
  a completion callback or a worker→procurement RPC), never the worker's
  own outbox pen.
- Pre-existing, unchanged (dev-notes 029–049): the procurement server
  test cleanup leak, the `dbmigrate` advisory lock, `go test -race`
  unrunnable locally (no cgo), the gofmt CRLF working-copy listing.
