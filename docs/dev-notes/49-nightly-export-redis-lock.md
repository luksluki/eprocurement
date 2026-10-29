# Dev note — #49 Nightly export job with Redis lock

- **Issue:** #49
- **Branch:** feature/49-nightly-export-redis-lock
- **Date:** 2026-10-27

> **Provenance:** all four open questions were answered by the maintainer
> on 2026-10-27 (issue #49 comments) before implementation: (1) CSV content
> and object key pinned — PO register, trailing 24 h, columns
> `po_number, vendor_code, vendor_name, currency, total_cents, status,
> issued_at`, key `exports/{tenant_id}/{yyyy-mm-dd}/purchase-orders.csv`;
> (2) `github.com/redis/go-redis/v9` approved as the only new dependency,
> with the schedule deliberately parsed by stdlib (no cron library);
> (3) the missed-run metric (gauge `export_age_seconds`) deferred to #55 —
> #49 detects via the run-history flag + a warning log; (4) single
> env-level schedule (`EXPORT_SCHEDULE=01:00`, `EXPORT_TZ=UTC`) with tenants
> enumerated via `SELECT DISTINCT tenant_id` from
> `procurement.purchase_orders` — no app-schema tenants table (the PRD §16
> "01:00 tenant-local" deviation is documented below).

## What changed

- **`services/worker/export` (new)** — the nightly export (#49, FR-10,
  PRD §16), one package per concern:
  - **`schedule.go`** — `ParseSchedule("HH:MM", IANA zone)` with the
    standard library only; `Daily.Next`/`Previous` compute the fire slots
    in the zone's wall clock (AddDate, so DST behaves like cron); the
    package **imports `_ "time/tzdata"`** — the runtime image is
    `alpine:3.20` without the tzdata package
    (deploy/docker/service.Dockerfile), so without the embed only
    `UTC`/`Local` could ever load inside the container.
  - **`lock.go`** — the Redis surface: `Acquire` = SET NX PX with a random
    16-byte hex token, `Release` = the Lua compare-and-delete script (a
    holder that lost its lock to TTL never deletes the new holder's),
    plus the heartbeat get/set. Keys per the PRD §13 families, per tenant:
    `lock:export:nightly:{tenant}`, `job:export:last_success:{tenant}`
    (heartbeat value = RFC 3339). `RedisKV` wraps go-redis v9; the `KV`
    interface keeps the scheduler/tracker unit-testable.
  - **`csv.go`** — RFC 4180 via `encoding/csv` with `UseCRLF`, UTF-8, the
    pinned header, RFC 3339 UTC `issued_at`; an empty register writes a
    header-only file (a valid "no POs today").
  - **`exporter.go`** — the PO register: `Register.Tenants`
    (`SELECT DISTINCT tenant_id`) and `Register.PurchaseOrders`
    (half-open `[fire−24h, fire)` anchored on `issued_at`, drafts excluded
    by the NULL, `ORDER BY po.number`, vendors joined for code/name) —
    both through `tenant.WithBypass` (the outbox.Pending precedent: the
    transaction drops to `eproc_app` with the bypass flag, never the raw
    superuser pool). `Exporter.Export` builds the CSV in memory and Puts
    it to MinIO under `ObjectKey` = `exports/{tenant}/{yyyy-mm-dd}/
    purchase-orders.csv` with the date from the fire time **in EXPORT_TZ**;
    the same key per date makes a re-run overwrite idempotently.
  - **`runs.go`** — the `Tracker`: missed-run detection (heartbeat age >
    26 h **OR** the previous slot without a successful run; the first-ever
    run is the cold-start exception), the fire-time flag propagation into
    the run record, the boot-time `DetectAndFlag` (marks the newest
    record), the success heartbeat stamp, and the failed-run record.
    Detection failures degrade to "not missed" — the export still runs.
  - **`scheduler.go`** — `NewScheduler` validates fail-fast (TTL > 0,
    budget > 0, **TTL strictly > budget** — otherwise error, never a
    silently-overlapping scheduler; malformed schedule/zone = error) and
    `Run` = boot detection sweep, then the fire loop (sleep to next slot,
    fire, repeat; ctx cancel returns nil). Per tenant: acquire the lock —
    the loser skips (log only, **no run record**; the run belongs to the
    winner), the winner detects, exports under the job budget
    (`context.WithTimeout`), and records the outcome — `status=failed` +
    `error_message` on PG/MinIO failure or budget exhaustion (the record
    is written on a fresh context: a deadline-exceeded run must still be
    recorded, #53's error display reads it), `status=success` + heartbeat
    on success.
- **`services/documents/export` (new)** — the run history store: MongoDB
  `export_runs`, one document per tenant per slot, **upserted** on
  `{tenant_id, scheduled_for}` (`uniq_tenant_slot` unique index; a re-run
  overwrites and regenerates `run_id`) — not append-only like
  events/audit_log. Pinned indexes + `tenant_slot {tenant_id: 1,
  scheduled_for: -1}`; `UpsertRun`, `Latest` (cold-start nil),
  `HasSuccess(from, to)` (half-open range — tolerant of late-waking
  fires), `MarkNewestMissedPrevious`, and the read side `List` (newest
  first, limit default 20/max 100) / `Get` (uuid-validated,
  tenant-scoped — cross-tenant indistinguishable from missing). The
  worker writes it, documents serves it (the #47 notifications
  precedent: the worker projects, documents owns).
- **proto + gen + documents API** — additive `ListExportRuns` /
  `GetExportDownloadUrl` RPCs with the `ExportRun` message (no
  `object_key` on the wire — the presigned URL is the only download
  path, the attachment convention). `methodPermissions` maps both to
  `rbac.ExportRun` — **the first real consumer of `export:run`**
  (finance/admin; defense in depth, the gateway enforces the same
  permission). Handlers: list passes the caller tenant; download
  rejects non-successful runs with `412 export_not_succeeded` and
  presigns a 15-minute GET. The gen files were regenerated with the
  `gen-proto` target's exact protoc invocation (proto_path=proto + the
  module options); `git status` shows only the documents gen pair and the
  proto changed.
- **`services/documents/objectstore/minio.go`** — `Put(ctx, key, reader,
  size, contentType)`: the export writer's store primitive. **Bucket
  pin:** the worker writes and documents presigns against the **single
  `S3_BUCKET`** (default `attachments`), CSV under the `exports/`
  prefix — there is deliberately **no second bucket env**; a separate
  export bucket would 404 every presign (the writer and presigner must
  agree).
- **gateway** — `DocumentsClient` widened with the two export RPCs (the
  fake in `attachments_test.go` follows); new `exports.go`:
  `GET /api/v1/exports/runs?limit=` and `GET /api/v1/exports/runs/:id/
  download-url`, both behind `RequirePermission(rbac.ExportRun)`, the
  flat download envelope (attachment convention), and the machine-code
  message map. `POST /api/v1/exports` (manual export, PRD §15) stays
  with #53.
- **worker config + main** — `REDIS_URL`, `EXPORT_SCHEDULE`,
  `EXPORT_TZ`, `EXPORT_LOCK_TTL` (15m), `EXPORT_JOB_BUDGET` (10m), and
  the `S3_*` writer knobs; boot dials Redis with the same 1 s → 10 s
  backoff as Mongo/RabbitMQ (no crash-loop, PRD §14.8), ensures the
  bucket and the `export_runs` indexes, then constructs the scheduler —
  a config violation stops boot. `runBoth` became `runAll` (publisher +
  consumers + scheduler under one signal context; one component's error
  cancels the siblings).
- **web** — `/app/reports` replaces its placeholder: `ExportRunsTable`
  (client) fetches `/api/exports/runs`, shows status / missed badge /
  the failed run's error message, and a Download CSV button that mints
  the URL via `/api/exports/runs/{id}/download-url` and navigates the
  browser straight to MinIO (bytes never stream through the BFF). The
  two BFF routes are thin proxies (the events-route pattern); 401 runs
  the refresh dance, 412/404 ride through with their machine codes.
  i18n: a `reportsTable` copy section (the Timeline pattern — client
  components receive copy as props), both locales.
- **compose/.env.example** — the worker service gains `REDIS_URL`,
  `S3_ENDPOINT: minio:9000` + credentials + `S3_BUCKET`, the `EXPORT_*`
  knobs, and `depends_on: redis, minio (service_healthy)`.
  `docker compose config` clean.

## Decisions

- **Overlap prevention, not repair.** The lock TTL must strictly exceed
  the job budget (fail-fast at construction) so a lock can never expire
  while its holder may still be working; a slot the lock loser skips is
  simply not run — no catch-up. Catch-up was rejected as ambiguous by
  design: the trailing-24 h window and the dated object key make "which
  slot was missed" ill-defined after the fact; manual execution is #53's.
- **Lock-not-acquired writes no run record.** The record describes the
  run that happened; the loser's "run" is a skip with a log line.
  Symmetrically, a *failed* run (PG/MinIO down, budget exceeded) IS
  recorded — `status=failed` + `error_message` — because that is the
  signal #53's error display and the operator need.
- **Missed-run detection at boot AND at fire, without waiting 24 h.**
  Boot: sweep every enumerated tenant and flag the newest record +
  warning log — a worker that was down surfaces the miss the moment it
  is back (PRD §14.8 "Restart → missed-run detection alerts"). Fire: the
  detection result is carried by the run record being created
  (`missed_previous=true`). Two signals, OR-ed: heartbeat age > 26 h
  (24 h slot + 2 h grace for slow runs and skew) or an empty previous
  slot. A missing heartbeat (flushed Redis) falls back to the slot
  history; a tenant with no records is the cold-start exception. The
  Prometheus gauge stays with #55 per the maintainer decision.
- **Single bucket, `exports/` prefix — pinned.** Writer (worker) and
  presigner (documents) share `S3_BUCKET`; a second bucket env would
  break every presign with a 404. Documented in
  docs/schema/documents.md so no future implementer splits them.
- **`time/tzdata` embedded in the worker.** The alpine runtime has no
  zoneinfo; `LoadLocation("Asia/Jakarta")` fails there without the
  embed and scheduler construction would reject every non-UTC `EXPORT_TZ`
  at boot. The blank import lives in the export package, so only the
  worker binary carries the ~450 KB zone database.
- **PRD §16 deviation — one schedule, not per-tenant local time.**
  "01:00 tenant-local" needs a per-tenant timezone source; the app schema
  has no tenants table (only the seed's demo schema), and adding one is
  an architecture change outside #49 (maintainer decision 4). #49 ships
  one env-level schedule; all tenants fire at the same instant, and the
  object-key date is computed per `EXPORT_TZ`. A per-tenant schedule can
  land later behind the same scheduler.
- **Run history in Mongo, not Postgres, and no outbox events.** ADR-0001:
  no shared tables, no dual writes — `procurement` owns business state and
  is the single outbox writer; the worker must not write the procurement
  outbox, so the §14.6 `export.completed` / `export.failed` events are
  deliberately NOT emitted in #49 (a conscious gap for #53, which owns
  the manual-export flow and can decide whether an export run belongs in
  the event catalog and who publishes it). The run history is a
  documents-owned Mongo projection the worker writes — exactly the #47
  notifications shape.
- **Two Redis test tiers.** Unit tests fake the `KV` interface; the
  overlap AC is proven by `TestRedisLockTwoParallelAcquirers` against a
  real Redis, gated on `TEST_REDIS_URL` and skipped otherwise — the #47
  convention. CI has no Redis service (postgres/mongo/minio only,
  .github/workflows/ci.yml) and is NOT changed in #49: adding a CI
  service container is an infrastructure change under golden rule 10 and
  is proposed as a named follow-up instead.

## Evidence

All commands run from the repo root against the local compose stack
(PostgreSQL on :55432, MongoDB :27017, MinIO :9000, Redis :6379 — `TEST_*`
values derived from `.env`, the CI names):

- `go vet ./... && go build ./...` — clean.
- `TEST_DATABASE_URL=… TEST_MONGO_URI=… TEST_S3_ENDPOINT=localhost:9000
  TEST_REDIS_URL=redis://localhost:6379/0 go test
  ./services/worker/export/ -count=1 -v` — green, 21 tests: schedule
  parsing table (incl. `Asia/Jakarta`, malformed zone/schedule, empty
  zone), Next/Previous slot arithmetic (fire at the slot, before it,
  Jakarta↔UTC instants, `SlotDate` rollover), CSV (pinned header, RFC
  4180 quoting of commas and doubled quotes, CRLF, header-only empty
  register), object key per zone, `TestNewSchedulerValidation` (TTL ==
  budget, TTL < budget, zero TTL, zero budget, bad schedule, bad zone —
  all constructor errors; defaults 15m > 10m), lock loser and Redis
  error write no record, success records + heartbeat + lock release,
  export failure records `status=failed` with the message, budget
  exceeded (50 ms budget vs 300 ms export) still records the failed run,
  the detection table (cold start, healthy, exactly-26 h boundary,
  stale heartbeat, absent heartbeat × empty/healthy slot), the slot
  range asked at fire vs boot, boot flagging, and the fire loop.
- `TEST_REDIS_URL=redis://localhost:6379/0 go test
  ./services/worker/export/ -count=1 -run TestRedis -v` — green against
  the compose Redis: **`TestRedisLockTwoParallelAcquirers` — two racing
  acquirers, exactly one winner, the winner's token in Redis** (the
  overlap AC), acquire/release lifecycle incl. the foreign-token
  compare-and-del no-op and re-acquire after TTL expiry, and the
  heartbeat roundtrip.
- `TEST_DATABASE_URL=… TEST_S3_ENDPOINT=… go test
  ./services/worker/export/ -count=1 -run "TestRegister|TestExporter" -v`
  — green: the window table (in/out, edge at −24 h+1 s in, exactly at
  fire out, draft NULL out, another tenant's isolation, ORDER BY
  po_number, row fidelity), the full export against real MinIO (stored
  content type `text/csv; charset=utf-8`, the exact CSV bytes, re-run
  same key), and the header-only empty register.
- `TEST_MONGO_URI=… go test ./services/documents/export/ -count=1` —
  green: one record per slot (upsert overwrite), the detection inputs
  (Latest cold start + per-tenant, HasSuccess half-open), boot flagging
  (idempotent, other tenants untouched), Get validation + cross-tenant
  indistinguishable, limit clamping.
- `TEST_MONGO_URI=… TEST_S3_ENDPOINT=… go test
  ./services/documents/server/ ./services/documents/objectstore/ -count=1`
  — green, including the new `TestExportRunsListAndDownload`
  (finance lists newest-first with the missed flag, downloads the exact
  CSV bytes through the presigned URL, failed run → 412
  `export_not_succeeded`, cross-tenant id → 404 `export_run_not_found`,
  malformed id → 400) and `TestExportRunsRBAC` (buyer →
  `permission_denied: requires export:run` on both methods, anonymous →
  Unauthenticated), plus `TestMinIOPutAndOverwrite` (content type, same
  key overwrite serves the latest bytes).
- `go test ./services/gateway/server/ -count=1` — green, including the
  new `TestExportRunsRoute` (list passthrough, limit forward, tenant
  metadata), `TestExportRunsRouteRequiresPermission` (buyer → 403
  `permission_denied` + `required: export:run` on both routes, no
  documents call), `TestExportDownloadURLRoute` (presigned envelope;
  cross-tenant 404, failed 412, malformed 400 with machine codes).
- `pnpm vitest run` (apps/web) — 205 passed, including the 15 new: the
  two BFF routes (401 without cookie and no gateway call, bearer
  forwarding, 401/403/412/404 passthrough with codes, 502 on network)
  and `ExportRunsTable` (statuses + missed badge + error message render,
  download click navigates `window.location` to the presigned URL,
  per-row failure state, empty state, retryable error state).
- `docker compose config` — clean; `docker compose build worker documents
  gateway && docker compose up -d --wait` — all services healthy, the
  worker now depends on redis + minio (`docker compose ps`: worker Up,
  redis/minio healthy).
- **Non-UTC schedule in the container (the tzdata AC):**
  `docker compose run --rm -e EXPORT_TZ=Asia/Jakarta worker` boots clean
  and logs `export scheduler armed … next_fire 2026-10-28T18:00:00Z tz
  Asia/Jakarta` (= 2026-10-29 01:00 Jakarta) — the zone loads inside
  alpine:3.20 only because of the embedded tzdata.
- **Live end-to-end fire:** a one-off worker
  (`EXPORT_SCHEDULE=<now+90s> EXPORT_TZ=UTC`) enumerated both tenants of
  the local DB and logged `export run succeeded … rows 1, object_key
  exports/{tenant}/2026-10-27/purchase-orders.csv`; the `attachments`
  bucket holds both objects (read back via in-container `mc`:
  `po_number,…\r\nPO-000001,G-INV39-A,PT Invoice,IDR,15450000,
  acknowledged,2026-10-27T12:49:30Z` — the pinned format byte for byte);
  Mongo `export_runs` carries the success record; Redis holds the
  heartbeat and no lock (released). Through the live gateway:
  `POST /api/v1/auth/login` (acme admin) → `GET /api/v1/exports/runs`
  returns the tenant's run → `GET …/runs/{id}/download-url` returns the
  presigned GET on `localhost:9000/attachments/exports/…` → `curl` of
  that URL streams the exact CSV; the same admin requesting the OTHER
  tenant's run id gets `404 export_run_not_found`. (The acme demo PO used
  for this check was inserted directly into the local demo DB — it is
  local data, not repo content.)
- `gofmt -w` applied to every touched Go file; `go vet ./...` re-run
  clean. Full `go test ./...` + `pnpm build/lint/test` gates re-run by
  the workflow after this dev note, per the ask.

## Follow-ups

- **#53 (manual export):** `POST /api/v1/exports` (PRD §15), the run
  error display built on this PR's `error_message`, and the decision on
  the §14.6 `export.completed`/`export.failed` events (who publishes
  them, given the worker may not write the procurement outbox — the
  conscious gap above).
- **#55 (Prometheus/Grafana):** the `export_age_seconds` gauge + alert
  (heartbeat age), deferred per the maintainer decision; the detection
  data (heartbeat + run history) is in place.
- **CI Redis service:** the overlap integration test skips in CI (no
  Redis service, .github/workflows/ci.yml) per the #47 convention; adding
  a `redis:7.4-alpine` service container + `TEST_REDIS_URL` env is a
  small CI change that needs maintainer sign-off (golden rule 10) —
  proposed as its own follow-up.
- **Per-tenant schedules** (PRD §16 "tenant-local 01:00") need a
  per-tenant timezone source (a tenants table or a setting) — an
  architecture change for a future issue; the scheduler's `Daily` is
  ready to be constructed per tenant.
- **Reaper move:** cron now lives in the worker (#49), but the
  orphan-upload reaper stays in documents until an issue of its own
  moves it (docs/schema/documents.md says so explicitly).
- Pre-existing, unchanged (dev-notes 029–039): the procurement server
  test cleanup leak, the `dbmigrate` advisory lock, `go test -race`
  unrunnable locally (no cgo).
