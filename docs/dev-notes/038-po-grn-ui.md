# 038 — PO and GRN UI

- **Issue:** #38
- **Branch:** feature/38-po-grn-ui
- **Date:** 2026-10-29

> **Provenance:** the maintainer was unavailable while #38 was prepared, so
> the decisions below were taken by the agent — the dev-notes 035/036/037
> protocol, open to override in PR review and flagged in the PR description.

## What

- **Proto + gen** — additive `ListPurchaseOrders` /
  `GetPurchaseOrder` RPCs with `ListPurchaseOrders{Request,Response}` (the
  requisition list envelope shape: header-only rows documented in the proto
  comments) and `GetPurchaseOrderRequest`; `make gen-proto` regenerated only
  the procurement gen files.
- **`services/procurement/purchaseorder`** — the #38 filter model in the
  domain (`ListFilter`/`ValidateList`: seven-status whitelist, page/page_size
  clamping 1/20/max 100 with `MaxPage` bound, query trimmed + uppercased with
  escaped LIKE wildcards; sentinels `po_status_invalid` / `po_page_invalid`)
  and the store reads: `Get` (header + items under `tenant.WithTenant`, no
  lock — the requisition `Get` twin) and `List` (#32's query shape verbatim:
  page subquery `ORDER BY number COLLATE "C" DESC LIMIT/OFFSET` + exact COUNT
  as an uncorrelated scalar subquery / InitPlan + past-the-end COUNT
  fallback; rows header-only).
- **Migration `0011_purchase_order_list_indexes.sql`** —
  `purchase_orders_tenant_status_idx (tenant_id, status)`, direct parity with
  `requisitions_tenant_status_idx` (0003), carrying the status-filter
  shape. `migrations.go` needed no change (the `//go:embed *.sql` glob picks
  the file up).
- **procurement server** — both RPCs mapped to `rbac.PORead` in
  `methodPermissions` (every role per docs/rbac.md); thin `api.go` handlers;
  `mapPurchaseOrderError` gains the two list-code arms (InvalidArgument).
- **gateway** — `ProcurementClient` widened with the two calls;
  `GET /api/v1/purchase-orders` (`RequirePermission(rbac.PORead)`, envelope
  `{purchase_orders, total, page, page_size}`) and
  `GET /api/v1/purchase-orders/{id}` behind the same permission;
  `poMessages` gains the `po_status_invalid` / `po_page_invalid` human
  sentences.
- **web `/app/purchase-orders`** — the PRD §11 list replacing the
  PlaceholderPage: a **server component** on the design system
  (`DataTable`/`TablePagination`/`Badge`/`PageHeader`/`EmptyState`/
  `ErrorState`), filters (q / status / page_size) in the URL through a plain
  **GET `<form>`** — no client JavaScript required — with the number column
  linking to the detail and status badges over the seven statuses.
- **web `/app/purchase-orders/[id]`** — the detail (the #34 requisition-detail
  pattern): header (number, status badge, vendor, currency, total with
  `maximumFractionDigits: 0`, issued/created), items table (description,
  qty_ordered, qty_received, remaining, unit price, line total), links back
  to the list and to the source requisition, and a not-found state on gateway
  404. `POActions` (issue/acknowledge) and `GoodsReceiptForm` render only
  when the pure eligibility helper allows.
- **`lib/po-eligibility.ts`** — the #34 approval-eligibility pattern for POs:
  `holdsPOWrite`/`holdsGRNPost` (buyer/admin) and `canIssuePO` (draft),
  `canAcknowledgePO` (issued), `canPostGoodsReceipt`
  (acknowledged/partially_received) mirroring the #36/#37 domain gates; the
  server keeps enforcing every gate.
- **`GoodsReceiptForm`** — per-line qty inputs (disabled when a line is full)
  + notes; client validation without a round trip (whole qty > 0, ≤ the
  line's remaining, ≥ 1 filled line, notes ≤ 1000); success shows the
  `GRN-%06d` number and `router.refresh()`; machine codes map to localized
  (ID+EN) inline messages, with the pinned `grn_over_receipt` detail parsed
  so the received/requested/ordered numbers appear in the message; 401 runs
  the refresh dance.
- **BFF proxies** — `POST /api/purchase-orders/[id]/{issue,acknowledge}` and
  `.../goods-receipts` (the latter mirrors the shape validation: non-empty
  lines, integer qty > 0, unique po_item_ids, notes bound — gateway 4xx ride
  through verbatim, 401 passthrough, 502 otherwise).
- **i18n** — new `purchaseOrders` (list) and `purchaseOrderDetail` (detail,
  actions, receipt form incl. the machine-code tables and the
  over-receipt/qty-over template functions) dictionary sections, ID + EN.
- **Docs** — `docs/schema/procurement.md` (the "Purchase order list —
  filters, pagination, query plans (#38)" section with the 0011 index, the
  fixture's FK dependencies, the measured budget table and plan summaries;
  follow-ups list updated), `docs/rbac.md` (#38 landed entry).

## Decisions

- **List ordering pinned `number DESC`** (newest first) — the #32 contract
  and its UI copy; numbers are sequential per tenant so number order is
  creation order. Documented in the proto comments and the schema doc.
- **The scale fixture seeds its dependencies** (plan-review correction):
  `purchase_orders.requisition_id` is a FK to `requisitions` under
  `UNIQUE (tenant_id, requisition_id)` and `vendor_id` a FK to `vendors`, so
  "100k POs in one tenant" is impossible without 100k requisitions (one per
  PO, deterministic `eeeeee38-…` ids) and one vendor; the cleanup deletes
  POs BEFORE requisitions, then both counters and the vendor (FK-safe
  order). The #32 requisition fixture had no such dependencies.
- **Four filter shapes measured** (unfiltered / status / prefix /
  prefix+status) — the #27/#32 convention; the plan named only three and the
  review restored the prefix-only shape.
- **The list page is a server component with a GET form**, not the #32
  TanStack-Query client table: the issue's UI box names DataTable/
  TablePagination and "tanpa JS wajib", and the vendors page (#27/#51) is
  the in-repo precedent. One page per request; the client bundle for the
  route stays at 168 B.
- **The over-receipt 409 keeps its numbers** (plan-review correction): the
  gateway's dedicated renderer carries the pinned detail line verbatim in
  `error`; the BFF forwards it, and `parseOverReceipt` extracts
  received/requested/ordered so the localized inline message states them —
  the exact figures the per-line error needs, instead of a flattened
  sentence.
- **Only issue + acknowledge joined the detail page** — the AC flow names
  those two actions; cancel/close stay REST-only (#36's routes) rather than
  widening the UI diff.
- **Bootstrap data is explicit because the seed is unreachable** (plan
  correction): `scripts/seed` writes only the isolated `demo` schema and its
  PO status pool has no `draft`, so live proof needs useradmin users, an
  approval rule, an approved PR, an active vendor, and the #35 REST
  conversion — the dev-note 037 recipe (executed, see Evidence).
- **Dictionary copy crossing into client components is template strings,
  not functions** (live-proof finding): `qtyOver`/`overReceipt` started as
  `(n) => string` functions in the receipt copy and crashed the
  acknowledged re-render — React cannot serialize function props across the
  server→client boundary. The copy is now placeholder templates
  (`"{remaining}"`, `"{received}/{requested}/{ordered}"`) interpolated by
  the client's `fill`. (The vendors `pagination.summary` function is fine —
  TablePagination renders server-side.)
- **The receipt form resets its per-line state when fresh items arrive**
  (live-proof finding): a one-shot `useState(initial)` kept pre-receipt
  remainders and leftover quantities after `router.refresh()`, letting a
  second submit replay an already-received line (the server's cumulative
  guard caught it — mapped inline — but the state was wrong). The state now
  keys on a generation (item ids + received quantities) and rebuilds on
  change; regression-tested via rerender with post-receipt items.

## Evidence

Against the local compose PostgreSQL (`TEST_DATABASE_URL` derived from
`.env`, packages run sequentially per the dev-note 031 dbmigrate protocol;
the pre-existing `TestVendorGRPCList` L27 residue was wiped before the
DB-gated runs):

- `make gen-proto` — only `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}` and the proto changed (`git status --short`;
  documents gen files re-touched line-endings only and were reverted).
- `go build ./... && go vet ./...` — clean (re-run after every step).
- `go test ./services/procurement/purchaseorder/ -count=1` — green:
  `TestValidateList` (happy/normalization table incl. all seven statuses,
  wildcard escaping, defaults; failure arms `po_status_invalid` /
  `po_page_invalid`), `TestStoreListFilters` (descending order, header-only
  rows, status/prefix/literal-%/combined filters, cross-tenant invisibility),
  `TestStoreListPagination` (deep page, past-the-end total fallback,
  defaults), `TestStoreGet` (aggregate with item ids, cross-tenant
  `ErrNotFound`, non-UUID `ErrIDInvalid`), plus the #35–#37 suites unchanged.
- `TestStoreListScale100k` — **passed 3 consecutive runs** (budget p95 < 300
  ms, 4 shapes × 30 timed runs each after 3 warm-ups):
  - run 1: unfiltered p95 ~104 ms / status ~16 ms / prefix ~43 ms / combined
    ~57 ms;
  - run 2: ~104 / ~19 / ~44 / ~56 ms;
  - run 3: ~102 / ~15 / ~50 / ~56 ms.
  EXPLAIN (ANALYZE, BUFFERS) captured in the test log: the status shape runs
  `Index Only Scan` (count arm) and `Bitmap Index Scan` (page arm) on the new
  `purchase_orders_tenant_status_idx` — the exact shape 0011 exists for; the
  unfiltered count arm is an Index Only Scan on `purchase_orders_tenant_idx`
  with `Heap Fetches: 0`; prefix shapes seq-scan (the #27/#32 RLS bypass-arm
  trade-off). Summaries mirrored in docs/schema/procurement.md.
- `go test ./services/procurement/server/ -count=1` — green, including the
  new `TestPurchaseOrderGRPCListAndGet` (defaults newest-first header-only,
  status/prefix/combined/past-the-end, cross-tenant listing, requester may
  list where writes are buyer-only, `po_status_invalid`/`po_page_invalid`
  400s with exact messages, detail with item ids, cross-tenant 404
  `po_not_found`, bad id 400, anonymous Unauthenticated on both methods) and
  the #35–#40 suites unchanged.
- `go test ./services/gateway/server/ -count=1` — green, including the new
  `TestPurchaseOrderReadRoutesThroughServerNew` (wired via `WithProcurement`;
  viewer passes `po:read`; envelope `{purchase_orders,total,page,page_size}`;
  forwarded query params; claims as gRPC metadata; detail with item ids),
  `TestPurchaseOrderReadRoutesRequireBearer` (401 anonymous before any
  procurement call), `TestPurchaseOrderReadRouteErrors`
  (`po_status_invalid`/`po_page_invalid` → 400 and `po_not_found` → 404 with
  machine codes + human sentences), and the #35–#40 suites unchanged.
- Web (vitest, `apps/web`): `npx vitest run` — **242/242 green** (190
  pre-existing + 52 new across 7 files: `po-eligibility` hidden/shown tables
  for the three actions; `purchase-order-filters` parse/serialize/clamping/
  unknown-status drops incl. the requisition-status negative;
  `POActions.test.tsx` (hidden on null, POST + refresh, 409 mapping, generic
  fallback, 401 dance); `GoodsReceiptForm.test.tsx` (per-field validation
  without a network call — zero/over-remaining/fractional/empty/over-long
  notes, disabled full line, POST body of filled lines with trimmed notes,
  GRN-number confirmation + refresh, over-receipt 409 with the parsed
  numbers, po_illegal_transition mapping, 401 dance, `parseOverReceipt`
  unit); the three BFF route tests (401 without cookie and no gateway call,
  forwarding with bearer + local validation mirrors, pinned over-receipt 409
  passthrough, 401 passthrough, 502)).
- `npx next build` — green; new routes in the table: `/app/purchase-orders`
  (168 B client), `/app/purchase-orders/[id]` (2.37 kB client),
  `/api/purchase-orders/[id]/{issue,acknowledge,goods-receipts}`.
  `npx eslint .` — clean.
- Full `go test ./... -count=1 -p 1` with `TEST_DATABASE_URL` — **green**
  after the fixture fix (the first run failed twice for environmental
  reasons, both reproduced and explained: the scale shapes measured while a
  concurrent `docker compose build` saturated the machine — prefix p95
  949 ms vs ~44 ms quiet — and the gRPC list test's `G-PO38-*` vendor codes
  were outside the `G-PO35-%` cleanup family so a repeat run hit
  `vendor_duplicate_code`; codes renamed into the covered family, residue
  wiped, quiet re-run all green). Residue wiped after the run per the
  dev-note 031 protocol.
- Root gates: `pnpm install --frozen-lockfile` (up to date), `pnpm build`
  (3/3), `pnpm lint` (3/3), `pnpm test` (6/6, incl. the assert-ui-css gate)
  — all green. `go vet ./... && go build ./...` clean.
- **Live browser proof of the buyer PO→GRN flow (the issue's first AC
  box), run against the compose stack** with `gateway` + `procurement`
  rebuilt from this branch (`docker compose up -d --build gateway
  procurement` — the dev-note 034 stale-image lesson; procurement applied
  0011 on boot, `purchase_orders_tenant_status_idx` verified in
  `pg_indexes`) and the web app on `next start` with
  `GATEWAY_URL=http://localhost:8080`:
  - **Bootstrap (the seed is unreachable — scripts/seed writes only the
    isolated `demo` schema):** `buyer@acme.example` created via
    `services/identity/cmd/useradmin` (role buyer, tenant
    `11111111-…`); the dev-note 034 demo pair
    `requester@acme.example`/`approver@acme.example` was still present
    (passwords reset through useradmin's upsert) with the IDR approval rule
    and active vendor `V-DEMO49` already on the tenant. PR-000019
    submitted by the requester and approved by the approver over the API,
    then converted by the buyer (`POST from-requisition` → draft
    PO-000001, the only live PO besides the demo `PO-DEMO49`).
  - **List** (`/app/purchase-orders`): the GET form (Cari nomor / Status —
    seven options / Ukuran halaman / Terapkan), both PO rows with status
    badges, money formatting (`Rp 154.500`), number links, and the
    pagination summary ("Menampilkan 1–2 dari 2 pesanan pembelian") all
    rendered as the buyer. `GET /api/v1/purchase-orders?status=paused`
    answered `400 po_status_invalid` with the human sentence; `?q=po-0000`
    matched the case-insensitive prefix (total 1).
  - **Detail → issue → acknowledge:** PO-000001 draft showed the
    Terbitkan button (buyer + draft → eligible); clicking it moved the PO
    to `issued` with `Diterbitkan` stamped and the button switched to
    Konfirmasi pemasok; clicking that moved it to `acknowledged` — each
    via `POST /api/purchase-orders/[id]/{issue,acknowledge}` and
    `router.refresh()`.
  - **Partial GRN → completion:** on the acknowledged PO the GRN form
    rendered (per-line inputs, notes). A partial receipt posted →
    confirmation banner "Penerimaan tercatat: GRN-000001" (DB:
    GRN-000001, 1 line, Laptop 2/2) and the PO moved to
    `partially_received`; the completing receipt (Mouse 3, notes
    "Pengiriman lengkap batch kedua") → GRN-000002 and the PO `received`
    with both lines full and the form gone (received is not receiptable).
    The outbox shows the exact sequence: `po.created → po.issued →
    po.acknowledged → grn.posted × 2` on entity `purchase_order`.
  - **Inline validation, live:** an over-remaining quantity (Mouse 4 with
    3 remaining) rendered the per-line message "Jumlah melebihi sisa
    baris ini (sisa 3)." plus the form-level min-one-line message — no
    network call. A replayed stale quantity hit the **server** guard and
    mapped the pinned 409 detail inline: "Kelebihan penerimaan: baris ini
    sudah diterima 2 dan diminta 2 lagi, padahal dipesan 2." (the parsed
    `grn_over_receipt` numbers).
  - The proof caught **two real bugs**, both fixed and unit-tested before
    the final runs (see Decisions): functions in the receipt copy props
    crossed the RSC boundary (server-side exception on the acknowledged
    re-render), and the form kept stale per-line state after
    `router.refresh()`.

### Bootstrap recipe (as executed above)

1. `docker compose up -d --build gateway procurement` — rebuild from this
   branch (the stale-image lesson of dev-note 034); procurement applies
   0011 on boot.
2. Users via `services/identity/cmd/useradmin` (the #34 approver
   precedent; `-role` repeatable): a buyer, plus the requester/approver
   pair if the demo tenant's dev-note 034 users are gone.
3. One approved PR: the IDR approval rule (admin-only API), requester
   creates + submits, approver decides (UI #34 or API).
4. An active vendor, then `POST /api/v1/purchase-orders/from-requisition/{id}`
   as the buyer → PO draft → the `/app/purchase-orders` flow:
   issue → acknowledge → partial GRN → (attempted over-receipt) →
   completing GRN.

## Follow-ups

- The GRN-number confirmation banner unmounts with the form when the
  completing receipt moves the PO to `received` (the whole receiptable
  block disappears) — it stays visible on partial receipts (GRN-000001
  live). A toast or a persisted last-receipt note would keep it on
  completion.
- The over-receipt inline error is form-level today (the server names the
  offending po_item; the form could highlight that line's input
  specifically).
- Demo data added to the local stack: `buyer@acme.example` (password
  `demo-password`), password resets for the dev-note 034
  requester/approver pair, PR-000019 (approved), PO-000001 (received,
  GRN-000001/000002) — useful for the invoice issues; wipe with a reset if
  unwanted.
- **CI round 1 (run 40797112295) failed in the Go job — root cause fixed
  before the retry:** the *vendor* package's pre-existing
  `TestStoreListScale100k` (#27) measured p95 322 ms (median 209 ms vs ~73
  ms solo) and bust its 300 ms budget, because `go test ./...` runs the
  three 100k budget fixtures (vendor #27, requisition #32, and this issue's)
  **concurrently** against one database and their bulk-load/IO spikes landed
  inside vendor's first timed shape (this PR added the third fixture — the
  pre-existing pair fit). Fix: the three scale tests now coordinate through
  a shared PostgreSQL advisory lock taken **per heavy unit** — every seed
  batch (seeds now commit one transaction per batch), the ANALYZE/VACUUM/
  CHECKPOINT hygiene, and each measurement shape — so at most one such unit
  runs at a time without serializing whole tests (a whole-test lock would
  exceed the 10 m go-test budget: the solo fixtures run 150–450 s). Verified
  with the CI topology — the three packages run concurrently: vendor
  459.7 s, requisition 464.9 s, purchaseorder 246.7 s, all green.
- The gRPC list test's vendor fixture rides the shared `G-PO35-*` code
  family so `grpcCleanupPOVendor` covers it (a first draft used `G-PO38-*`
  and a repeat run hit `vendor_duplicate_code`). Its final wipe is a defer
  ordered BEFORE the pool closes: the file's usual `t.Cleanup` wipes run
  after `defer cleanup()` has closed the pool and fail silently (the
  documented pre-existing leak; other tests absorb it via their leading
  wipes, but this test is the file's last) — verified: zero vendors/POs
  residue after a solo run.
- `grn.posted` remains outside the worker's notification routing keys
  (dev-note 037 decision 4) — unchanged here.
- Pre-existing, unchanged (dev-notes 029/030/031/035/036/037): the
  procurement server test cleanup leak (L27/G-01 residue, hence `-p 1` and
  the before/after wipes), the `dbmigrate` advisory lock, `go test -race`
  unrunnable locally (no cgo).
