# 037 — Goods receipts with partial receipt rules

- **Issue:** #37
- **Branch:** feature/37-goods-receipts-partial-rules
- **Date:** 2026-10-27

> **Provenance:** the maintainer was unavailable while #37 was prepared
> (issue #37 has no comments), so the four decisions below were taken by
> the agent — the same protocol as dev-notes 035/036, open to override in
> PR review and flagged in the PR description:
>
> 1. **A further partial receipt on a `partially_received` PO is handled by
>    an explicit same-status branch, not a self-loop edge.** The #36
>    ten-edge contract (pinned in docs/schema/procurement.md: "every other
>    move — self-loops, skips … — is `po_illegal_transition`") is
>    unchanged. `purchaseorder.ApplyReceipt` therefore never calls a
>    transition method for partially_received → still-partially-received:
>    no aggregate event, no UPDATE on `purchase_orders` at all
>    (status/updated_at/updated_by untouched) — the receipt's effect lives
>    in `po_items.qty_received`, the GRN rows, and the `grn.posted` outbox
>    row. Without this branch every second partial receipt would answer
>    409 from `MarkPartiallyReceived`, breaking FR-07's core.
> 2. **The over-receipt detail line and a dedicated REST renderer.** The
>    gRPC message is pinned as
>    `grn_over_receipt: po_item=<uuid> received=<r> requested=<q> ordered=<o>`
>    — machine code as the strict prefix before the first colon, one
>    violating line (the first in request-line order, deterministic). The
>    goods-receipt route renders it through its own error renderer (not the
>    shared messages map): body `code` = the token before the colon
>    (exactly `grn_over_receipt`, stable for clients matching on equality),
>    body `error` = the full detailed message, 409 via the shared
>    `Aborted` mapping. Every other code on the route keeps flowing through
>    the shared `grpcHTTPError` unchanged.
> 3. **No Idempotency-Key mechanism on POST goods-receipts** — a conscious
>    deviation from the submit (#29/#31) and decide (#31) precedent.
>    Idempotent replay is hard to define for a cumulative quantity flow
>    (a retried partial that still fits under `qty_ordered` would currently
>    record twice), and the issue text does not ask for it. Follow-up
>    candidate: a decide-key-style table (the 0006 pattern) naming one
>    receipt per key.
> 4. **`grn.posted` is not in the worker's `NotificationsRoutingKeys`**
>    (services/worker/topology/topology.go — the actor-facing subset), so
>    goods receipts emit no in-app notification — including for finance
>    ahead of #38. The event still lands in `documents.history` (bound `#`)
>    and on the `grn.*` topic bindings per §14.5. Adding it means a
>    topology change plus a consumer decision; out of #37's scope, flagged
>    here as a follow-up.

## What

- **Migration `0008_goods_receipts.sql`** — `procurement.goods_receipts`
  (tenant-scoped `GRN-%06d` number unique per tenant; FK `po_id` without
  cascade — a PO with receipts must not be deletable; `notes` ≤ 1000 chars
  enforced in the domain), `procurement.goods_receipt_items` (`grn_id` FK
  CASCADE, `po_item_id` FK, CHECK `qty_received > 0`, UNIQUE
  `(grn_id, po_item_id)`), and `procurement.grn_counters` (the 0004/0007
  counter-table pattern — increments roll back with a rejected post, so
  numbers are gap-free). RLS tenant-scoped on all three (items derive
  tenancy from the parent GRN transitively); grants SELECT/INSERT/UPDATE
  for `eproc_app`. Loaded automatically by the `//go:embed *.sql` in
  migrations.go.
- **`services/procurement/purchaseorder`** — the aggregate method
  `ApplyReceipt(now, actor, lines)`: line membership (`grn_po_item_unknown`),
  duplicate lines (`grn_po_item_duplicate`), the cumulative over-receipt
  guard (`grn_over_receipt`, first violating line in request order), the
  status edge (acknowledged/partially_received only), the `qty_received`
  advances, and the three-branch target-status selection (decision 1). New
  `ErrOverReceipt` sentinel + typed `*OverReceiptError` whose `Error()` is
  the pinned detail line (decision 2) and which unwraps to the sentinel.
  `Item` gains `ID`; `loadItems` loads it; `loadForUpdate` is exported as
  `LoadForUpdate` (the `requisition.LoadForUpdate` precedent) so the
  goods-receipt store shares the PO row lock with every lifecycle
  transition — the serialization point the issue's technical note names.
- **`services/procurement/goodsreceipt` (new)** — the domain
  (`goodsreceipt.go`: request-shape validation before any transaction
  opens — non-empty lines, qty > 0, unique line ids, notes bound) and the
  store (`store.go`): `Post` runs the pinned step order in one tenant
  transaction — validate shape → lock PO (`purchaseorder.LoadForUpdate`) →
  `ApplyReceipt` → allocate `GRN-%06d` from the counter (after the guards,
  so a rejection burns no number) → insert GRN + items → advance
  `po_items.qty_received` with a `WHERE qty_received + $2 <= qty_ordered`
  DB backstop under the lock → guarded `purchase_orders.status` UPDATE only
  when the status changed → exactly one `grn.posted` outbox row (entity =
  `purchase_order`/PO id, actor + trace from the context, version 1,
  payload `{grn_id, number, po_number, po_status, items: [{po_item_id,
  qty_received}], received_by}`).
- **proto + gen + wiring** — additive `PostGoodsReceipt` RPC with
  `GoodsReceipt`/`GoodsReceiptItem`/request/response messages and
  `PurchaseOrderItem.id` (field 5); `make gen-proto` regenerated only the
  procurement gen files. procurement `server.New` takes the new store
  (main.go constructs it); `methodPermissions` maps the RPC to
  `rbac.GRNPost`; `api.go` adds the handler, `goodsReceiptPB`, and
  `mapGoodsReceiptError` (the typed over-receipt error carries its pinned
  message verbatim; shape codes → 400, guards → Aborted/409, not-found →
  404). `purchaseOrderPB` now renders item ids.
- **gateway** — `ProcurementClient` widened with `PostGoodsReceipt` (the
  compile-forced fake in requisitions_test.go gained the stub); new
  `goodsreceipt.go`: route `POST /api/v1/purchase-orders/{id}/goods-receipts`
  behind `RequirePermission(rbac.GRNPost)` (PRD §15), body
  `{items: [{po_item_id, qty_received}], notes}` trimmed at the edge,
  **201 + the GRN with the updated PO embedded**, the dedicated over-receipt
  renderer (decision 2) and a messages map for the route's other codes.
  `purchaseOrderJSON` items expose `id`.
- **Docs** — `docs/schema/procurement.md` (new "Goods receipts (#37)"
  section: tables, store contract with the three branches, the pinned
  `grn.posted` payload, **the pinned over-receipt error format**, error
  codes, the entity-typing decision; the #36 section's "receipt transitions
  write no outbox rows" line updated to point at `grn.posted`; follow-ups
  list), `docs/rbac.md` (#37 landed entry).

## Decisions

- **Guard order: line membership → cumulative over-receipt → status edge,
  all before any mutation** (the `Cancel` guard precedent). The
  cumulative-before-edge order is pinned: a receipt against a `received`
  PO answers `grn_over_receipt` (every line is full), not
  `po_illegal_transition` — verified in both the domain and gRPC tests.
- **`grn.posted` on every branch, typed to the purchase order** — the
  entity columns name the PO (the `pr.step_approved` precedent, #31) so the
  FR-11 PO timeline shows every receipt; the GRN identity rides in the
  payload. The same-status branch writes no `purchase_orders` columns, so
  the outbox row is the branch's only "something happened" record besides
  the GRN rows themselves.
- **The DB backstop renders the pinned detail too** — the
  `qty_received + $2 <= qty_ordered` UPDATE guard is unreachable under the
  lock + domain guard, but if it ever fires it SELECTs the line's values
  and returns the same typed `OverReceiptError`, so even that path answers
  the pinned format rather than a generic 500.
- **Number allocation after the guards** (the #35 conversion's ordering):
  an over-receipt or illegal-status rejection burns no `GRN-%06d`; the
  store test proves the first success after a batch of failures is still
  `GRN-000001`.
- **`po_id_invalid` (not a grn-prefixed code) for a malformed path id** —
  the route lives under `/purchase-orders/{id}/…`; its id names a PO
  exactly like the #36 action routes, and consistency there wins.
- **Tests key PO lines by `qty_ordered`, never by slice index** — po_items
  rows of one conversion share `created_at`, so load order is not the
  basket order (the dev-note 035 multiset note); the first draft of the
  store tests keyed `po.Items[0]` and was flaky exactly there.

## Evidence

Against the local compose PostgreSQL (`TEST_DATABASE_URL` derived from
`.env`, packages run sequentially per the dev-note 031 dbmigrate protocol;
the pre-existing `TestVendorGRPCList` L27 residue was wiped before and
after, as in 035/036):

- `make gen-proto` — only `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}` and the proto changed (`git status --short`).
- `go build ./... && go vet ./...` — clean (re-run after every step below).
- `go test ./services/procurement/goodsreceipt/ -count=1` — green: the
  domain suite (`TestValidate`: happy path, empty basket, zero/negative
  qty, duplicate line, notes over bound) and the store suites —
  `TestStorePostGoodsReceiptBranches` (all three branches through the real
  flow — rules → PR → submit → approve → convert → issue → acknowledge;
  first partial `GRN-000001` → partially_received; second partial
  same-status with the `purchase_orders` status/updated_at/updated_by
  columns byte-identical before/after yet a second GRN + second
  `grn.posted` row; completion → received with both lines full; the
  `grn.posted` row asserted on entity `purchase_order`/PO id, actor,
  context trace, version 1, and the full pinned payload including
  `po_status`), `TestStorePostGoodsReceiptGuards` (over-receipt answers
  the pinned detail line exactly with zero GRN rows / outbox rows / qty
  moves; empty/qty/duplicate/unknown-line/notes/id/not-found table; draft
  PO → `po_illegal_transition`; nothing burned — the next success is still
  `GRN-000001`; cross-tenant → `po_not_found`),
  `TestStoreGoodsReceiptConcurrencyAndNumbering` (2 racers of 6 on a
  10-unit line → exactly 1 win + 1 over-receipt loss, qty 6, one GRN, one
  outbox row; 2 further parallel partials of 2+2 that fit both commit →
  3 GRNs + 3 `grn.posted` rows, PO received; tenant B's first GRN is its
  own `GRN-000001`).
- `go test ./services/procurement/purchaseorder/ -count=1` — green: the
  #35/#36 suites unchanged plus `TestApplyReceiptBranches` (the four
  branch cases, asserting the same-status case appends no event and does
  not move UpdatedAt/UpdatedBy) and `TestApplyReceiptGuards` (the exact
  pinned detail string for over-receipt, first-violating-line
  determinism, unknown/duplicate lines, illegal statuses, and the
  cumulative-before-edge answer on a received PO).
- `go test ./services/procurement/server/ -count=1` — green, including
  `TestGoodsReceiptGRPC` (requester/approver/finance/viewer →
  PermissionDenied; `po_id_invalid`/`grn_items_empty` → 400;
  `po_not_found` → 404; over-receipt → Aborted with the exact pinned
  message and no outbox row; draft receipt → `po_illegal_transition`;
  happy path with `x-trace-id` → `GRN-000001`, PO partially_received, one
  `grn.posted` row with actor/trace/`purchase_order` entity and the pinned
  payload; second partial same-status with a second row; completion →
  received; receipt on the received PO → over-receipt not
  illegal-transition; cross-tenant → 404; anonymous → Unauthenticated)
  and `TestMapGoodsReceiptError` (the full mapping table, typed + bare
  over-receipt arms).
- `go test ./services/gateway/server/ -count=1` — green, including the
  new `TestGoodsReceiptRouteThroughServerNew` (route wired via
  `WithProcurement`, buyer 201, GRN JSON with the updated PO embedded —
  item ids and advanced qty exposed; path id + trimmed item ids/notes
  forwarded; claims as gRPC metadata),
  `TestGoodsReceiptRouteRequiresPermission`
  (requester/approver/finance/viewer → 403 with `required: grn:post`
  before any procurement call; admin passes), and
  `TestGoodsReceiptRouteErrors` (over-receipt → 409 with `code` exactly
  `grn_over_receipt` AND `error` carrying the full detail line; the
  route's other codes keep the shared mapping with human sentences;
  malformed JSON → 400 `body_invalid` without a procurement call).
- Affected packages sequential sweep:
  `go test ./services/procurement/goodsreceipt/
  ./services/procurement/purchaseorder/ ./services/procurement/server/
  ./services/gateway/server/ -count=1 -p 1` — all green in one run from a
  cleaned database. (The first sweep attempt hit the pre-existing
  `TestVendorGRPCList` L27 residue — reproduced on dirty-database residue
  only, wiped per protocol, suite green from clean.)
- `docker compose exec postgres psql … \d procurement.goods_receipts` (and
  the items/counters twins) — 0008 applied with the documented columns,
  `UNIQUE (tenant_id, number)`, `UNIQUE (grn_id, po_item_id)`, the
  `qty_received > 0` CHECK, both FKs (CASCADE on `grn_id` only), forced
  RLS with the transitive items policy, and 12 grant rows for `eproc_app`;
  `procurement.schema_migrations` lists `goods_receipts`.
- `gofmt -w` applied to every touched Go file; `go vet ./...` re-run clean.
- pnpm build/lint/test not run by me (no TypeScript touched); the workflow
  script re-runs the full gate set after this dev note, per the ask.
- No compose or seed changes.

## Follow-ups

- **Idempotency-Key on POST goods-receipts** (decision 3): a decide-key
  style table (the 0006 `decision_idempotency` pattern) naming one receipt
  per key; until then a client retry of a partial that still fits records
  twice.
- **`grn.posted` in `NotificationsRoutingKeys`** (decision 4): needs a
  topology change plus a consumer/recipient decision (finance ahead of
  #38 is the obvious stakeholder); out of #37's scope.
- GRN read endpoints (list per PO / detail) arrive with the invoice 3-way
  match (#38) or a history issue; the posting response is currently the
  only GRN read.
- The conversion response (#35) still carries no item ids (only the locked
  loads do); harmless today — a client must issue + acknowledge before
  receiving anyway — but a PO get/detail endpoint should include them.
- Pre-existing, unchanged (dev-notes 029/030/031/035/036): the procurement
  server test cleanup leak (L27/G-01 residue, hence `-p 1` and the
  before/after wipes), the `dbmigrate` advisory lock, `go test -race`
  unrunnable locally (no cgo).
