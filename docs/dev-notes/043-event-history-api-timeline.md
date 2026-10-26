# 043 — Event history API and shared timeline component

- **Issue:** #43
- **Branch:** feature/43-event-history-api-timeline
- **Date:** 2026-10-26

## What

- **Gateway `GET /api/v1/events`** (new, `services/gateway/server/events.go`):
  proxies documents' `ListEvents` (#42) under Bearer auth +
  `RequirePermission(rbac.RequisitionRead)` — the exact permission documents
  re-checks in its `methodPermissions` (services/documents/server/server.go:34),
  so edge and service agree on the audience. The route registers in
  `server.New` whenever `WithDocuments` is set (documents only; no procurement
  needed). Query params `entityType`/`entityId`/`limit`/`cursor` are trimmed
  and forwarded; a non-numeric `limit` becomes 0 (documents normalizes to the
  default 20). The tenant never comes from the query string — it is taken from
  the verified claims and injected as gRPC metadata via `forwardContext`
  (same path as vendors/requisitions). Response envelope:
  `{events: [event_id, tenant_id, type, version, occurred_at, actor_id,
  trace_id, entity_type, entity_id, payload_json], next_cursor}`.
  `DocumentsClient` (services/gateway/server/attachments.go) gained
  `ListEvents`; the fake in `attachments_test.go` grew the method.
- **Web BFF `GET /api/events`** (new, `apps/web/src/app/api/events/route.ts`):
  thin proxy following the requisitions route — access token stays in the
  httpOnly cookie, `eventsToSearchParams` (`lib/events-query.ts`, new —
  Next.js typed routes reject non-handler exports from route files)
  canonicalizes the query (keeps entityType/entityId/cursor trimmed-non-empty
  and a positive-integer limit, drops everything else), 401 passes through
  for the refresh dance, **400 passes through with the gateway's machine code
  intact** (`events_filter_invalid` / `events_cursor_invalid`), anything else
  → 502 `gateway_unavailable`. `lib/gateway.ts` gained `EventEntry`/
  `EventsPage`/`listEvents`.
- **`lib/relative-time.ts`** (new): pure `formatRelativeTime(iso, locale,
  now?)` on `Intl.RelativeTimeFormat(numeric: "auto")` — deterministic and
  unit-tested in both UI locales; unparseable timestamps pass through so a
  broken row never blanks.
- **`components/Timeline.tsx`** (new, client): the shared timeline, generic
  over `entityType`/`entityId` (PR today; PO/invoice later — the host page
  stays with #34). `useInfiniteQuery` walks the keyset `next_cursor` ("Load
  more"); polling `refetchInterval: 2000` keeps the view inside FR-11's 2 s
  budget; events render newest-first with relative timestamps; the type
  filter is client-side over the loaded events; empty / skeleton /
  error-with-retry states; a 401 runs the refresh dance exactly like
  `RequisitionsTable` (`window.location.assign('/api/auth/refresh?next=…')`).
  Copy arrives as props (client components never import the i18n
  dictionaries).

## Decisions

- **Keyset cursor on the REST route — deviating from the repo's page/total
  convention, inherited from #42.** `next_cursor` is opaque and empty on the
  last page; there is no `total`. Dev-note 042 asked for exactly this note on
  the gateway route: a timeline is unbounded and read-while-writing, offset
  pagination skips/duplicates rows when new events land between pages, and
  `total` is meaningless on an append-only stream. The BFF (`/api/events`)
  and `lib/gateway.ts` (`EventsPage`) carry the same shape.
- **Query params are camelCase (`entityType`, `entityId`) because PRD §15
  spells the route `GET /api/v1/events?entityType=&entityId=`.** Every other
  route uses snake_case (`page_size`, `unit_price_cents`). The PRD wins over
  the local convention here — the route is quoted verbatim in the PRD's API
  surface — and the deviation is pinned in tests on both sides of the BFF so
  it cannot drift silently. New routes should stay snake_case.
- **The type filter is client-side** (a `<select>` over the loaded events)
  because #42's `ListEvents` has no type filter — only entity_type+entity_id.
  Adding a server-side filter would grow the documents API for one UI; the
  client already holds every loaded row. Revisit (server-side `type=` filter)
  when timelines routinely exceed a few pages or the event catalog grows.
- **Polling `refetchInterval: 2000` (FR-11 "within 2 s") is proven UI/API-side
  with vitest fake timers**, per the maintainer ruling recorded in issue #43
  comments: the action→event pipeline (#45 outbox publisher → #46 RabbitMQ →
  #47 worker consumer) is not merged, so live end-to-end latency cannot be
  demonstrated yet. `vi.advanceTimersByTimeAsync` shows `/api/events` is
  fetched again exactly when the clock crosses 2000 ms (1999 ms → still one
  call; +1 ms → second call). `useInfiniteQuery` + `refetchInterval`
  refetches every loaded page per tick — correct (keyset cursors are stable
  under concurrent inserts) and cheap at timeline page sizes; noted as a
  follow-up if timelines grow large.
- **BFF passes gateway 400s through with the machine code** instead of
  collapsing them into 502: `events_filter_invalid` /
  `events_cursor_invalid` are typed API errors (AGENTS.md: typed errors at
  API boundaries), and a bad cursor is a client anomaly worth distinguishing
  from "gateway unreachable". 401 stays a passthrough for the refresh dance.
- **Gateway tests reuse the `attachments_test.go` fake** rather than a new
  one: same package, same `DocumentsClient` interface — the fake gained
  `listEventsReq/Resp/Err` fields. The denial test signs an unknown role
  ("auditor"): every seeded role holds `requisition:read`, and `rbac.Check`
  grants unknown roles nothing (same trick as dev-note 042).
- **The BFF query canonicalizer lives in `lib/events-query.ts`, not in the
  route file**: Next.js typed routes (`next build`) reject any export from a
  `route.ts` that is not a handler/config symbol — the first build failed on
  `export function eventsToSearchParams` until it moved to the lib (same
  shape as `lib/requisition-filters.ts`).

## Evidence

All commands run on this branch against the compose stack (gateway replaced
locally — see below; documents/identity/procurement/mongo from compose).

- `go vet ./services/gateway/... && go test ./services/gateway/server/
  -count=1` — green; new tests: `TestListEventsReturnsEnvelopeAndCursor`
  (full envelope incl. `payload_json` + `next_cursor`; camelCase params
  forwarded verbatim; tenant + role metadata propagated from claims),
  `TestListEventsDefaultsLimitAndTrimsParams`, `TestListEventsValidationErrors`
  (400 + machine code + human message for `events_filter_invalid` /
  `events_cursor_invalid`), `TestListEventsRequirePermission` (unknown role →
  403 `permission_denied`, documents not called; missing token → 401),
  `TestEventsRegisteredThroughNew` (route live with `WithDocuments` alone).
- `pnpm vitest run src/lib/relative-time.test.ts` — 5 tests green; pins id/en
  strings incl. word forms ("bulan lalu"/"last month", "dalam 20 detik"/"in
  20 seconds"), the 60 s→1 min step-up, half-away-from-zero rounding, and
  pass-through of unparseable input.
- `pnpm vitest run src/app/api/events/route.test.ts` — 7 tests green:
  401-without-cookie (no gateway call), canonical forwarding with bearer,
  garbage dropped, 400 machine-code passthrough, 401 passthrough, 502 on
  5xx/network.
- `pnpm vitest run src/components/Timeline.test.tsx` — 8 tests green:
  newest-first order + relative timestamps + query forwarding, client-side
  type filter, keyset Load more (second call carries `cursor=…`, last page
  hides the button), empty state, skeleton while first page pending,
  error + retry, 401 refresh dance (`window.location.assign(
  '/api/auth/refresh?next=/app/requisitions/r-1')`), and the FR-11 polling
  proof with fake timers (1999 ms → 1 call; +1 ms → 2nd call to
  `/api/events?entityType=requisition&entityId=r-1&limit=20`).
- **Live verification against #42 seed data** (compose mongo + documents;
  the new gateway run locally via `PORT=8095 go run
  ./services/gateway/cmd/gateway` — port 8090 was taken; identity user
  `events43@demo.example` created on the seeded tenant
  `b14b843e-df61-4588-b0d3-f96fe7dc8c6d` via `cmd/useradmin`; seed run:
  `go run ./scripts/seed --tenants 1 --vendors-per-tenant 3 --requisitions
  30 --events 300`):
  - `curl "http://localhost:8095/api/v1/events?entityType=requisition&
    entityId=9052c7a0-0a72-4f68-933a-f556257df5d6"` → 7 events, newest-first
    (08:04:40 → 08:00:00), full envelope fields populated (actor_id, trace_id,
    payload_json), `next_cursor: ""`.
  - `&limit=2` → 2 events + non-empty base64 cursor; page 2 with
    `&cursor=…` → the next 2 events (strict continuation, no overlap).
  - `?entityType=requisition` (half filter) → **400
    `{"code":"events_filter_invalid", …}`**; `&cursor=garbage` → **400
    `{"code":"events_cursor_invalid", …}`**; no token → **401**.
  - Cross-tenant: an admin of tenant `22222222-…` requesting the same
    entity → `{"events":[],"next_cursor":""}` — no leak (scoping itself is
    documents' and was pinned in #42; this proves the gateway forwards only
    the claims tenant).
- Full gates I ran: `go vet ./... && go build ./...` — clean;
  `go test ./... -count=1` — whole repo green (DB-gated suites skip as
  designed); `pnpm lint` — clean; `pnpm test` — 16 files / 100 tests green +
  `assert-ui-css` ok; `pnpm build` — green (after moving
  `eventsToSearchParams` out of `route.ts`, see Decisions). `pnpm install
  --frozen-lockfile` not re-run (no dependency changes); the workflow script
  re-runs the full gate set after this dev note.

## Follow-ups

- **#45–#47 dependency (maintainer-ruled):** until the outbox publisher,
  RabbitMQ wiring, and the worker's history consumer merge, live events come
  only from the seed; the 2 s AC is proven UI/API-side (fake timers) and the
  live end-to-end demonstration follows once the pipeline merges.
- **The requisition detail page (#34, OPEN)** is the first host of this
  component — it passes `entityType="requisition"`, `entityId`, `locale`, and
  localized copy; a `timeline` dictionary section is still to be added there.
- Polling refetches every loaded page each 2 s tick (fine at 20-row pages);
  if timelines grow, refetch only page 1 and merge heads.
- Server-side type filter if timelines outgrow client-side filtering (see
  Decisions).
- Submit/create requisition events are still absent from the outbox
  (dev-note 031 follow-up) — the seed's `pr.created`/`pr.submitted` rows
  stand in for future traffic.
