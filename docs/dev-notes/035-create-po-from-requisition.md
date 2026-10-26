# 035 — Create PO from approved requisition

- **Issue:** #35
- **Branch:** feature/35-create-po-from-requisition
- **Date:** 2026-10-26

> **Provenance:** the maintainer was unavailable while #35 was prepared, so
> its open questions were decided by the agent — issue #35 comment 2
> ("Decision applied by agent (2026-10-26, maintainer unavailable — override
> welcome in PR review)"; not a maintainer answer). The applied decisions,
> all open to override in PR review and flagged in the PR description:
>
> 1. **PO initial status on conversion: `draft`.** Basis: PRD §15 (lines
>    353–354) separates `POST /purchase-orders/from-requisition/{id}` from
>    `POST /purchase-orders/{id}/issue`, and the demo script describes
>    "convert to PO → issue" as two steps. The issue transition, `issued_at`,
>    and the `po.issued` event therefore belong to #36 (PO lifecycle state
>    machine). #35 creates the draft, linked to the requisition, with its
>    creation event only. The seed's PO status list has no `draft` — a
>    seed-side note (demo rows), not a contract.
> 2. **"DB sequence per tenant" (FR-05 technical note) implemented as a
>    counter table (`po_counters`), not a native PostgreSQL SEQUENCE.**
>    `nextval()` is not transactional — it never rolls back — so a conversion
>    that fails after allocation (vendor deactivated, requisition not
>    approved) would burn a number and leave a gap in the per-tenant
>    sequence, which FR-05's "sequence per tenant" read as gap-free forbids.
>    The 0004 `requisition_counters` precedent made the same choice; #35
>    mirrors it.
>
> Both are presented in the PR description as open to override.

## What

- **Migration `0007_purchase_orders.sql`** — `procurement.purchase_orders`
  (tenant-scoped `PO-%06d` number unique per tenant; `UNIQUE (tenant_id,
  requisition_id)` = "cannot convert twice" as a database guarantee; CHECK
  with all seven FR-06 statuses from day one so #36 never needs an ALTER
  CHECK; `currency` + `total_cents` + audit fields — documented deltas vs
  the PRD §13 summary row), `procurement.po_items` (verbatim copy of the
  requisition lines; `qty_received` starts 0 for #37), and
  `procurement.po_counters` (the 0004 counter pattern). RLS on all three
  (items derive tenancy from the parent PO, like `requisition_items`);
  grants SELECT/INSERT/UPDATE. No ON DELETE CASCADE on `requisition_id` —
  financial records must not cascade away with a requisition.
- **`services/procurement/purchaseorder` (new)** — pure domain
  (`purchaseorder.go`: draft construction from copied requisition lines,
  total recomputed, the seven status names, machine-code sentinels) and the
  store (`store.go`): `CreateFromRequisition` runs the whole conversion in
  one tenant transaction with a pinned step order — lock requisition
  (`requisition.LoadForUpdate`) → replay lookup by (tenant_id,
  requisition_id) → **vendor gate on the transaction** → status gate
  (approved only) → `PO-%06d` allocation from the counter (after the gates,
  so failures burn no number) → guarded `approved → closed` (the domain edge
  #28 reserved for this) → insert PO + items → exactly one `po.created`
  outbox row via `outbox.AppendTx`. Table-driven domain tests plus DB-backed
  store tests: happy path, replay (different vendor_id ignored; still 200
  after the vendor was deactivated), gates with full rollback, cross-tenant
  denial, and the gap-free/concurrency evidence (below).
- **`vendor.ForUseTx(ctx, tx, id)`** (`services/procurement/vendor/store.go`)
  — the tx-scoped twin of #25's pool-based `GetForUse`, which opens its own
  transaction and cannot run inside the caller's. The conversion gates on it
  so a vendor cannot be deactivated between the check and the insert
  (anti-TOCTOU); exported like `requisition.LoadForUpdate` (#30 precedent).
  `GetForUse` itself is untouched (#25 contract).
- **proto + wiring** — additive `CreatePurchaseOrder` RPC with
  `PurchaseOrder`/`PurchaseOrderItem`/`CreatePurchaseOrderRequest` messages
  and a `CreatePurchaseOrderResponse{purchase_order, created}` wrapper (the
  `DecideApprovalResponse` pattern): `created` separates a fresh conversion
  from a replay; `gen/go` regenerated (`make gen-proto`, only procurement
  files changed). procurement `server.New` takes the new store (signature
  change; main.go constructs it); `methodPermissions` maps the RPC to
  `rbac.POWrite` (buyer/admin; the package needed no change). Gateway:
  `POST /api/v1/purchase-orders/from-requisition/{id}` (PRD §15) with
  `RequirePermission(rbac.POWrite)`, `vendor_id` trimmed at the edge,
  responses **201 on creation / 200 on replay** (the plan's pinned "replay
  tetap 200"), errors through the shared `grpcHTTPError` mapping
  (`poMessages` adds the human sentences).
- **Docs** — `docs/schema/procurement.md` (0007 section: tables, §13 deltas,
  the pinned conversion contract, `po.created` payload + §14.6 catalog
  extension, error codes, gap-free evidence location); `docs/rbac.md`
  (#35 landed entry).

## Decisions

- **Pinned path order, replay before vendor gate.** The replay lookup runs
  after the requisition lock and before any gate, so a replay returns the
  original PO even when the vendor has since been deactivated or the
  vendor_id in the request differs — "cannot convert twice" is an idempotent
  success, not an error. Concurrent conversions of one requisition serialize
  on the requisition row lock (one winner creates, the loser replays); the
  unique index is the belt-and-braces backstop.
- **`vendor_id` required in the request** (REST body and proto field). The
  requisition aggregate has no vendor (PRD §13); a conversion must name one.
  Empty/non-UUID → 400 `po_vendor_id_invalid` before the transaction opens
  (store-level `uuid.Parse`, mapped at the API layer).
- **`po.created` is an extension of the §14.6 catalog** — the catalog's
  first PO event is `po.issued` (#36's). Two segments so the §14.5 `po.*`
  bindings deliver it (same reasoning as #31's `pr.step_approved`). Pinned
  payload: `{number, requisition_id, requisition_number, vendor_id,
  total_cents, currency, created_by}` — `requisition_number` so the PO
  timeline (#43) can link back.
- **Conscious gap: the requisition close writes no requisition-side event.**
  Submit/cancel also write none today; only Decide writes `pr.*` rows. The
  requisition timeline (#43, already shipped) therefore does not show the
  conversion, despite FR-11's "every state change appears in document
  timeline". A `pr.converted`/`pr.closed` outbox event is a follow-up
  candidate for #36/#45. Flagged in the PR description.
- **No in-memory event log on the PO aggregate** — the creation event is the
  outbox row; #36 owns the lifecycle. (The requisition aggregate's
  in-memory log is already consumer-less — dev-note 031 follow-up — and is
  not replicated here.)
- **`po_items` ordering is multiset-stable, not sequence-stable**: rows in
  one conversion share `created_at` (transaction timestamp), exactly like
  `requisition_items`; loading orders by `created_at, id`. Copy-exactly-once
  is verified as a multiset.
- **Gap-free numbering evidence lives in the store tests, not the seed**:
  the seed writes POs straight into `demo.purchase_orders`
  (`scripts/seed/main.go` copyRows → `demo.*`), which never passes through
  the conversion path. `TestStoreNumberingAndConcurrency` proves: sequential
  conversions number `PO-000001..N`; a failed conversion burns no number
  (the next success stays sequential); concurrent conversions of different
  requisitions form one consecutive run; two tenants' counters are
  independent.

## Evidence

Against the local compose PostgreSQL (TEST_DATABASE_URL from `.env`),
packages run sequentially per the dev-note 031 dbmigrate note:

- `make gen-proto` — regenerated `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}`; `git diff --stat` shows only procurement files
  changed under `gen/` and `proto/`.
- `go build ./... && go vet ./...` — clean (run after all edits).
- `go test ./services/procurement/purchaseorder/ -count=1` — green:
  domain suite (draft construction, total recomputation, validation
  sentinels) and store suites —
  `TestStoreConvertApprovedRequisition` (real approval flow → draft
  PO-000001 with `created=true`, items copied verbatim, total = requisition
  total, requisition closed, one `po.created` row with pinned payload/actor;
  replay with a different vendor_id returns the original PO with
  `created=false`, still one row; replay after the vendor was deactivated
  still succeeds),
  `TestStoreConvertGates` (deactivated vendor → `ErrDeactivated` with full
  rollback: requisition still approved, no PO, no outbox row, next
  conversion still PO-000001; missing/cross-tenant vendor → `ErrNotFound`;
  draft/submitted → `ErrRequisitionNotApproved`; cross-tenant requisition →
  `requisition.ErrNotFound`; non-UUID ids fail fast),
  `TestStoreNumberingAndConcurrency` (PO-000001..4 sequential; 2 goroutines
  on one requisition → identical PO, 1 row, 1 event; 4 concurrent different
  requisitions → one consecutive run; tenant B's first PO is PO-000001;
  store round-trip of copied lines).
- `go test ./services/procurement/vendor/ -count=1 -run
  "TestForUseTx|TestDeactivatedVendorUnusableAndImmutable|TestCrossTenantIsolation"`
  — green: `ForUseTx` inside `tenant.WithTenant` (active passes,
  deactivated → `ErrDeactivated`, missing → `ErrNotFound`, tenant B's
  transaction cannot see tenant A's vendor). The full vendor suite (incl.
  the 100k scale test) ran green in the final sequential sweep below.
- `go test ./services/procurement/purchaseorder/ ./services/procurement/vendor/
  ./services/procurement/requisition/ ./services/procurement/approval/
  ./services/procurement/outbox/ ./services/procurement/server/
  -count=1 -p 1` — all green in one sequential run from a cleaned database
  (the dev-note 031 dbmigrate/leak protocol).
- `go test ./services/procurement/server/ -count=1` — green, including the
  new `TestPurchaseOrderGRPCConversion` (requester/approver/viewer →
  PermissionDenied; bad vendor id → 400; unknown requisition → 404; draft →
  409 `po_requisition_not_approved`; deactivated vendor → 412 with the
  requisition still approved; convert with `x-trace-id` → PO-000001 draft
  with copied lines and closed requisition, one `po.created` row with
  actor + trace + payload; replay with another vendor → original PO, one
  row; cross-tenant → 404) and `TestMapPurchaseOrderError`. Item assertions
  compare multisets — po_items rows in one conversion share `created_at`,
  so load order is not the requisition's line order (an index-based assert
  was flaky exactly there and was fixed).
- `go test ./services/gateway/server/ -count=1` — green, including the new
  `TestPurchaseOrderRouteThroughServerNew` (route wired via
  `WithProcurement`; buyer 201 with the draft PO JSON and 200 on the replay
  flag; path id + trimmed vendor_id forwarded; claims as gRPC metadata),
  `TestPurchaseOrderRouteRequiresPermission` (requester/approver/finance/
  viewer → 403 before any procurement call; admin passes),
  `TestPurchaseOrderRouteErrors` (409/412/404/400 with machine codes
  preserved; malformed JSON → 400 `body_invalid` without a procurement
  call).
- `gofmt -w` applied to every file this diff touches (the repo-wide
  `gofmt -l` noise is the pre-existing CRLF checkout, verified by stashing:
  untouched files list identically); `go build ./... && go vet ./...` clean
  after every step.
- Repeat-run check: a second `go test ./services/procurement/server/` on the
  dirty database fails at the **pre-existing** `TestVendorGRPCList` leak
  (dev-notes 029/031) — verified by stashing this diff and reproducing the
  identical failure on the old code (which also fails
  `TestRequisitionGRPCLifecycle` there). From a cleaned database every
  affected package passes; the residue this suite leaves (L27/G-01 vendors,
  now also G-PO35 vendors + PO rows, wiped by the PO test's leading
  cleanup) was removed after the run.
- `docker compose exec postgres psql … \d procurement.purchase_orders` —
  0007 applied with the documented columns, the two unique indexes, the
  seven-status CHECK, both FKs, and forced RLS; `po_items` and `po_counters`
  verified the same way.
- pnpm build/lint/test not run by me (no TypeScript touched); the workflow
  script re-runs the full gate set after this dev note, per the ask.
- No compose or seed changes.

## Follow-ups

- **#36** (PO lifecycle): issue/acknowledge/close/cancel transitions from
  `draft` — `issued_at`, `po.issued`/`po.acknowledged` events, and PRD §15's
  `POST /purchase-orders/{id}/issue`. The 0007 CHECK already accepts all
  seven statuses, so #36 needs no schema change.
- **Requisition-side conversion event** (`pr.converted`/`pr.closed`) so the
  requisition timeline (#43) shows the conversion — the conscious FR-11 gap
  above; candidate for #36/#45.
- PO read endpoints (get/list) arrive with the PO list/detail issues; the
  conversion response is currently the only PO read.
- Pre-existing, unchanged (dev-notes 029/030/031): the procurement server
  test cleanup leak (t.Cleanup runs after the deferred pool close — the new
  PO gRPC test inherits the same pattern and its leading cleanup), the
  `dbmigrate` advisory lock, `go test -race` unrunnable locally (no cgo).
