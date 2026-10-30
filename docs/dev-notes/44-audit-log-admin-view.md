# Dev note — #44 Decision audit log with actor IP and admin view

- **Issue:** #44
- **Branch:** feature/44-audit-log-admin-view
- **Date:** 2026-10-30

> **Provenance:** the three binding decisions came from the maintainer in
> the issue #44 comments: (1) the audit listing is **`audit:read` as the
> matrix already defines it** — viewer + admin, no role-set change; (2) the
> definition of "decision" follows **PRD §14.7's verb list**
> (approve/reject/issue/post/match); (3) **`actor_ip` extends the §14.5
> envelope** (the `entity_type`/`entity_id` precedent of #47). One agent
> judgment call is flagged below (XFF forwarding on reads vs. decisions).

## What changed

- **`services/documents/history`** — the audit projection completes the
  PRD §14.7 verb set. `AuditAction` now maps `po.issued → issue`,
  `grn.posted → post`, and `invoice.matched`/`invoice.discrepancy → match`
  beside the four decide types mapped since #31 — **eight decision event
  types over five verbs**. (The maintainer's issue comment says "7"; four
  of the eight landed with #31, which is why #44 itself adds only three
  new names for four types — the count differs by whether the
  `invoice.matched`/`invoice.discrepancy` pair is counted once or twice.)
  `po.acknowledged`, `po.cancelled`, `po.closed`, and `pr.submitted` stay
  non-decisions. `Event`/`AuditEntry` gain `ActorIP` (`NewEvent` signature
  +1 param — the seed and `api_test.go` call sites follow), `EventBSON`/
  `AuditBSON` store it as `actor_ip`, and decoding tolerates its absence
  (pre-#44 rows decode to `""`).
- **Read side** — `Store.ListAudit` (action filter validated against the
  five verbs → `audit_action_invalid`; entity pair both-or-neither; limit
  default 20/max 100; the same keyset cursor codec and `occurred_at DESC,
  event_id DESC` order as ListEvents; absolute tenant scoping) and
  `Store.ExportAudit` + `history.AuditCSV` (RFC 4180, CRLF, the pinned
  bounded column set `event_id, occurred_at, recorded_at, actor_id,
  actor_ip, action, entity_type, entity_id, trace_id`, capped at the newest
  `MaxAuditExportRows` = 10 000 rows per tenant). The audit collection
  stays insert-only: it is written by `Apply` alone and read by
  ListAudit/ExportAudit alone — **no update or delete path exists on the
  service surface or in the seed** (the seed bulk-inserts demo rows
  directly, PRD §17 "no update/delete API").
- **Proto + gen** — additive `ListAudit`/`ExportAudit` RPCs with the
  `AuditEntry` message (no `details` on the wire — the bounded column set;
  the same payload stays readable through ListEvents `payload_json`);
  `Event` gains `actor_ip` (field 7). `make gen-proto` regenerated only
  `gen/go/documents/v1` (the procurement gen churn was line-endings only
  and was reverted).
- **documents server** — both methods mapped to `rbac.AuditRead` in
  `methodPermissions` (viewer/admin — the permission's first real
  consumer); handlers read the caller tenant from auth metadata and map
  the typed store errors (`audit_action_invalid`,
  `events_filter_invalid`, `events_cursor_invalid` → InvalidArgument).
- **`services/procurement`** — migration **0013**
  `outbox.actor_ip text NOT NULL DEFAULT ''`; `outbox.Event.ActorIP` +
  `AppendTx` column + `PendingEvent.ActorIP` in the publisher read; all
  six outbox writers (decide, submit, conversion, lifecycle, GRN post,
  match) set `ActorIP: logger.ActorIP(ctx)` next to the trace id. No new
  index — actor_ip is never a query key.
- **IP propagation (end-to-end)** — the web BFF forwards the incoming
  request's `X-Forwarded-For` on the **decision** calls
  (`lib/gateway.ts` `callGateway` `forwardedFor`; the decide/issue/
  acknowledge/goods-receipts/match BFF routes pass
  `request.headers.get("x-forwarded-for")`); the gateway resolves `c.IP()`
  under fiber's `EnableTrustedProxyCheck` + `TrustedProxies` +
  `ProxyHeader: X-Forwarded-For` (v2.52.6, fields verified via
  `go doc`/source) from the new `GATEWAY_TRUSTED_PROXIES` env
  (comma-separated IPs/CIDRs; **empty = trust nothing**: every forwarded
  header is ignored and the socket peer is recorded); `forwardContext`
  appends it as `x-actor-ip` metadata (the `x-trace-id` pattern); the
  shared `TraceInterceptor` copies it into the context
  (`logger.WithActorIP`/`ActorIP`); the publisher envelope carries
  `actor_ip` onto RabbitMQ; the consumer maps it into `history.NewEvent`;
  Mongo `events`/`audit_log` and the `/api/v1/events` +
  `/api/v1/audit` REST responses expose it. The demo compose stack pins
  the backend network to `172.28.0.0/16` and sets the gateway's
  `GATEWAY_TRUSTED_PROXIES` to it — covering containers and the host's
  bridge-gateway address (published-port calls from the host-run web BFF);
  a direct caller from outside the range is recorded with its socket
  address.
- **gateway audit routes** — `services/gateway/server/audit.go`:
  `GET /api/v1/audit` (`{entries, next_cursor}`) and
  `GET /api/v1/audit/export` (text/csv,
  `Content-Disposition: attachment; filename="audit-log.csv"`), both behind
  `RequirePermission(rbac.AuditRead)`; the documents dial sets
  `grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16 MiB))` — the
  second layer of the export bound (bounded columns, 16 MiB, 10 000-row
  cap). `DocumentsClient` widened with the two RPCs (the
  `fakeDocumentsClient` follows).
- **web admin view** — `/app/audit` (server component, no client JS): the
  DataTable columns occurred_at / actor / action badge / entity / IP /
  trace, an action filter through a plain GET form, keyset pagination
  ("Older" continues from `next_cursor`, "Newest" resets), and an Export
  CSV link to the BFF route. The nav entry renders only for `audit:read`
  holders (`layout.tsx`: admin or viewer — the Admin group stays
  admin-only, so the page rides in `mainNav` behind the condition); the
  page itself answers a gateway 403 with a dedicated sentence for anyone
  reaching it by URL. i18n copy in both locales (`nav.audit`,
  `pages.audit`, `auditPage`), pinned by a new dictionary test. Two thin
  BFF proxies (`/api/audit`, `/api/audit/export`) keep the token
  server-side.
- **Docs** — `docs/schema/documents.md` (audit_log decision set, `actor_ip`
  field + trust note, the ListAudit/ExportAudit contracts and permission
  rows, the append-only scope sentence), `docs/schema/procurement.md`
  (migration 0013 section, the outbox column-table row, the envelope-delta
  note, follow-ups entry), `docs/rbac.md` (#44 landed entry).

## Decisions

- **Eight decision types, five verbs — and why the count says 7.** PRD
  §14.7 names five verbs; `invoice.matched` and `invoice.discrepancy` are
  the two event names of one match decision, so the decision **event-type**
  count is 8 while the issue comment's "7" counts the not-yet-mapped
  surface before #44 widened it — 4 of the 8 decision types had already
  landed with #31, and the matched/discrepancy pair collapses to one
  decision. #44 adds the 4 missing types (`po.issued`, `grn.posted`,
  `invoice.matched`, `invoice.discrepancy`).
- **`invoice.discrepancy` maps to `match`, not `reject`.** The event is
  the outcome of running the match decision; the verdict rides in
  `details`. A discrepancy is not a rejection of the invoice.
- **`details` is not on the audit wire.** The list and the export stay
  bounded by design; the payload is already readable per event through
  ListEvents `payload_json` (the timeline's job). This is what keeps the
  CSV's worst case a few MB.
- **`ExportAudit` is unary, not MinIO/streaming.** The three-layer bound
  (columns / 16 MiB / 10 000 rows) makes a single response safe and is the
  smallest diff; the #49 nightly-export pattern (write CSV to MinIO,
  presign the download) is the documented follow-up if the cap must rise.
- **Trusted-proxy trade-off, flagged for maintainer review.** Anyone
  inside `GATEWAY_TRUSTED_PROXIES` can spoof the `X-Forwarded-For` the
  gateway records as `actor_ip`. Accepted because `actor_id` — the JWT
  subject — is the authoritative attribution and the IP is display/context
  data; the PR description must flag this explicitly. The compose demo
  range deliberately includes the host bridge address so the host-run BFF
  works, which is exactly the spoofable surface being traded.
- **XFF forwards on decisions, not on reads** (the one agent judgment
  call): the new audit BFF routes are pure reads that write no audit rows,
  so they forward no XFF; the five decision routes do. This is the
  substance of the plan's "BFF route baru meneruskan x-forwarded-for"
  read against the actual dataflow.
- **`WithTrustedProxies` is always-on checking.** The fiber app enables
  `EnableTrustedProxyCheck` unconditionally and `ProxyHeader` names
  X-Forwarded-For; with no configured proxies every forwarded header is
  ignored (c.IP() = socket peer). This also hardens the pre-existing login
  limiter's per-IP key, which previously honored spoofable XFF by default.
- **No audit index change.** ListAudit sorts `occurred_at DESC,
  event_id DESC`; the existing `tenant_occurred {tenant_id, occurred_at}`
  serves the prefix and the tiebreaker resolves in a bounded top-K sort at
  page size — changing the pinned index set (dev-note 042) would break
  boot on already-indexed volumes for no measurable gain at demo scale.
- **`po.acknowledged` forwards XFF too** (uniform envelopes) but stays a
  non-decision — acknowledging an obligation is not one of §14.7's verbs.

## Evidence

All commands run from the repo root against the local compose stack
(postgres :55432, mongo :27017, rabbitmq :5672, minio :9000; `TEST_*`
values derived from `.env`, `TEST_S3_ENDPOINT` scheme-stripped):

- `make gen-proto` — only `gen/go/documents/v1/{documents.pb.go,
  documents_grpc.pb.go}` and the proto changed; the procurement gen pair
  showed line-ending-only churn (empty `git diff --numstat`) and was
  reverted.
- `go vet ./... && go build ./...` — clean. `gofmt -l` on touched files
  matches pristine HEAD's CRLF working-copy listing (verified via
  `git stash` round-trip); vet/build are the real signal, per dev-notes
  041/045.
- `go test ./... -count=1` with `TEST_DATABASE_URL` + `TEST_MONGO_URI` +
  `TEST_RABBITMQ_URL` + `TEST_S3_ENDPOINT` set — **whole repo green in one
  parallel run** (the CI shape). DB-gated/Mongo-gated suites included.
  (An earlier sweep failed only because I passed the S3 endpoint with its
  `http://` scheme — a harness typo, not a code failure; rerun clean.)
- `go test ./services/documents/history/ ./services/documents/server/ -count=1`
  (Mongo) — green, including the flipped `TestAuditAction` (8 decision
  types + 6 non-decisions), `TestAuditCSV` (pinned header + CRLF, no
  details leak), `TestAuditFilterNormalize`, `TestListAudit` (five-verb
  rows with actor_ip fidelity, action filter, keyset walk),
  `TestListAuditCrossTenantAndInvalid` (tenant B invisible; typed
  ErrActionInvalid/ErrFilterInvalid/ErrCursorInvalid),
  `TestExportAudit` (6 newest rows, tenant-scoped, CSV without details),
  and at the gRPC layer `TestListAuditAndExport` (order/filters/export
  header/machine codes) + `TestListAuditRBAC` (buyer → PermissionDenied
  `permission_denied: requires audit:read` on both methods, anonymous →
  Unauthenticated, viewer + admin read fine).
- `go test ./services/procurement/{outbox,approval,requisition,purchaseorder,goodsreceipt,match}/
  ./services/procurement/server/ ./internal/platform/... -count=1 -p 1` —
  green; `TestAppendTxRoundTrip` now asserts the `actor_ip` column
  round-trips and rides `Pending` out; `TestStoreDecideIdempotencyAndOutbox`
  asserts the decide rows carry `203.0.113.7` from
  `logger.WithActorIP(ctx)`; `TestTraceInterceptorPropagatesActorIP` pins
  trimmed-present / empty-absent.
- `go test ./services/gateway/server/ -count=1` — green, including
  `TestAuditListRoute` (envelope with actor_ip, filters trimmed,
  x-tenant-id metadata), `TestAuditRouteRequiresPermission` (buyer 403
  `required: audit:read` on both routes with no documents call;
  anonymous 401), `TestAuditExportRoute` (text/csv + attachment headers,
  bytes passthrough, 400 mapping), and `TestForwardContextActorIP` —
  **both trusted-proxy branches**: no trusted proxies → spoofed
  X-Forwarded-For ignored, socket peer `0.0.0.0` forwarded as x-actor-ip;
  peer trusted (`0.0.0.0/0`) → the forwarded `203.0.113.9` wins.
  `TestListEventsReturnsEnvelopeAndCursor` now pins `actor_ip` in the
  events REST shape.
- `go test ./services/worker/... -count=1` (RabbitMQ + DB + Mongo) —
  green; `TestEnvelopeFromRow` golden JSON updated with `"actor_ip"` and a
  decision-row assertion.
- **Migration 0013:** `DATABASE_URL=… go run ./services/procurement/cmd/migrate`
  → "procurement migrations up to date (applied 1)";
  `schema_migrations` tops at **13**; `\d procurement.outbox` lists
  `actor_ip` as the last column.
- **Seed:** `make seed` — `seeded … events=1000000 in 47.22s` (52 s wall,
  budget 2 min). Seeded Mongo: `events=1000000 audit_log=499535
  audit_with_ip=499535` (the wider decision set now audits 5 of the seed's
  10 event types, all with a deterministic 10.x actor_ip; a `match` row
  verified by hand). `make seed-reset` → `events=0 audit_log=0`.
- **Compose:** `docker compose config --quiet` — valid (pinned
  172.28.0.0/16 subnet + `GATEWAY_TRUSTED_PROXIES` on the gateway).
- **Web (apps/web):** `npx vitest run` — **356/356 green (50 files)**,
  including the new `/api/audit` + `/api/audit/export` route tests
  (401-without-call, canonical query forward, 400/401/403/502 mappings,
  CSV re-serving with attachment headers) and the extended i18n
  dictionary test. `npx eslint` on every touched path — clean.
  `npx next build` — green with the new routes in the table
  (`/app/audit`, `/api/audit`, `/api/audit/export`).
  `npx tsc --noEmit` — 0 errors outside test files; the 46 errors inside
  test files are the pre-existing mock-typing family (my new
  `route.test.ts` files add 2 of the same shape the events route's test
  already carries).
- Not run by me (the workflow script re-runs the full gate set per the
  ask): the root-level `pnpm install --frozen-lockfile && pnpm build &&
  pnpm lint && pnpm test` (I ran the app-level equivalents above) and
  `docker compose up -d --wait` over the full stack.

## Follow-ups

- **PR description must flag** the trusted-proxy spoofing trade-off (the
  compose range includes the host bridge address) and the §14.5 envelope
  delta `actor_ip` (PRD stays pinned; the #47 precedent).
- Streaming/MinIO export (the #49 pattern) if the 10 000-row cap must
  rise; the three-layer bound is documented so the next implementer knows
  which layer to lift.
- The `/app/audit` entity filter exists in the API (`entityType`/
  `entityId`) but is not surfaced in the UI — the action filter covers the
  demo need; add a per-aggregate drill-down (e.g. link from the
  requisition timeline) when a use case names it.
- Pre-existing, unchanged (dev-notes 029–041): the procurement server test
  cleanup leak (hence `-p 1` for DB-backed sweeps), the `dbmigrate`
  advisory-lock race, `go test -race` unrunnable locally (no cgo), and
  `gofmt -l`'s CRLF working-copy listing.
