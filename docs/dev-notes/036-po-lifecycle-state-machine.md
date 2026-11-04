# 036 — PO lifecycle state machine

- **Issue:** #36
- **Branch:** feature/36-po-lifecycle-state-machine
- **Date:** 2026-10-26

> **Provenance:** the maintainer was unavailable while #36 was prepared
> (issue #36 has no comments), so the two decisions below were taken by the
> agent — the same protocol as dev-note 035, open to override in PR review
> and flagged in the PR description:
>
> 1. **REST acknowledge/cancel/close ship with #36**, not only PRD §15's
>    `POST /purchase-orders/{id}/issue`. Basis: the issue's scope line
>    ("guards for each transition") needs an execution path per guard, and
>    FR-06 names the whole chain; a state machine without cancel/close
>    endpoints could not demonstrate the receipt rule the issue's acceptance
>    criteria call out. The extra routes follow the issue route's shape
>    exactly (path id, no body, `po:write`, 200 + updated PO).
> 2. **The edge set is FR-06's linear chain plus loose cancel and
>    short-close — exactly 10 edges.** FR-06's arrow chain
>    (issued → acknowledged → partially_received → received → closed, with
>    cancelled branching off) does not name every legal edge; the two
>    additions mirror the requisition machine's precedents (loose cancel was
>    maintainer decision 1 on #28; `Close`-from-receipts matches #28's
>    reserved-edge pattern). Notably `partially_received → cancelled` and
>    `issued → received`-style skips are illegal.

## What

- **Domain (`services/procurement/purchaseorder/purchaseorder.go`)** — the
  #36 lifecycle: a `transitionEvents` map pinning the ten legal edges, six
  transition methods (`Issue`, `Acknowledge`, `Cancel`, `Close`,
  `MarkPartiallyReceived`, `MarkReceived` — the receipt pair is owned and
  tested here, driven by #37; the #28 `Close` precedent), `Terminal()`, an
  in-memory event log (`Events()`/`PullEvents()`, one aggregate-local event
  per legal transition; creation still records none — dev-note 035), and the
  new sentinels `po_illegal_transition` / `po_has_receipts`. `Issue` stamps
  `IssuedAt`; `Cancel` guards the edge first, then the sum of
  `qty_received` over all lines.
- **Store (`store.go`)** — `Issue`/`Acknowledge`/`Cancel`/`Close` over one
  shared `transition` skeleton: one tenant transaction per action — load the
  PO (header + items) `FOR UPDATE` (the serialization point all lifecycle
  transitions share, and #37's receipts will share), run the domain method,
  land a guarded `UPDATE … WHERE id = $1 AND status = '<from>'` (issued_at
  written as the post-transition aggregate value), append exactly one
  outbox row. IO sentinels `po_id_invalid` / `po_not_found`; `wrap` passes
  all lifecycle sentinels through bare.
- **Events** — `po.issued` / `po.acknowledged` are §14.6 catalog entries;
  `po.cancelled` / `po.closed` extend it (two segments for the §14.5 `po.*`
  bindings — the documented #35/#31 extension pattern). Pinned
  `TransitionPayload`: `{number, requisition_id, vendor_id, status,
  total_cents, currency, performed_by}`. Envelope carries actor + trace, as
  ever.
- **proto + gen** — additive `IssuePurchaseOrder`, `AcknowledgePurchaseOrder`,
  `CancelPurchaseOrder`, `ClosePurchaseOrder` RPCs with `{id}` request
  messages, returning `PurchaseOrder`; `make gen-proto` regenerated only the
  procurement gen files.
- **procurement server** — the four RPCs mapped to `rbac.POWrite` in
  `methodPermissions`; four thin `api.go` handlers;
  `mapPurchaseOrderError` gains the lifecycle arms (ID-invalid → 400,
  not-found → 404, illegal-transition / has-receipts → `Aborted`, which the
  gateway renders as 409 — FR-06).
- **gateway** — `ProcurementClient` widened with the four calls; routes
  `POST /api/v1/purchase-orders/{id}/issue` (PRD §15) plus `/acknowledge`,
  `/cancel`, `/close` behind `RequirePermission(rbac.POWrite)`; a shared
  `poAction` handler (path id → RPC, 200 + updated PO, no request body);
  `poMessages` gains the four human sentences.
- **Docs** — `docs/schema/procurement.md` (new "PO lifecycle state machine
  (#36)" section: edges, guards, store contract, events + payload, error
  codes; 0007 intro and follow-ups updated), `docs/rbac.md` (#36 landed
  entry).
- **No migration** — 0007's CHECK already accepts all seven statuses
  (its day-one purpose); no compose or seed changes.

## Decisions

- **Edge check before receipt check in `Cancel`.** A cancel attempted on a
  partially_received/received PO answers `po_illegal_transition` (it has no
  cancel edge), not `po_has_receipts`; the receipt error is reserved for
  POs that *could* cancel but for the receipts (draft/issued/acknowledged
  with `qty_received > 0`). Both are 409; the split keeps each code's
  meaning exact. Both guards run before any mutation.
- **Receipt guard input is the line-sum, checked in the domain** — the
  store loads items under the row lock and the domain re-derives the sum,
  so the guard cannot drift from the data the UPDATE will commit. A single
  advanced line pins the PO.
- **One generic guarded UPDATE for all four actions** —
  `SET status, issued_at, updated_at, updated_by WHERE id AND status =
  '<from-observed-under-lock>'`. The from-status comes from the locked
  load (stable under `FOR UPDATE`), making the WHERE clause the
  belt-and-braces twin of the requisition submit guard; `issued_at` is
  written as the post-transition aggregate value (now on issue, the stamped
  time afterwards, nil only for a draft cancel).
- **`po.cancelled` / `po.closed` as catalog extensions**, pinned in
  `docs/schema/procurement.md` with the payload — same reasoning and
  documentation pattern as `po.created` (#35) and `pr.step_approved` (#31).
- **No in-memory creation event** — dev-note 035's decision stands; #36's
  log records lifecycle transitions only.
- **The receipt edges write no outbox rows yet** — #37 owns the goods-receipt
  event shape (`grn.posted` is the §14.6 entry); adding a `po.received`
  event now would be speculative.

## Evidence

Against the local compose PostgreSQL (`TEST_DATABASE_URL` derived from
`.env`, packages run sequentially per the dev-note 031 dbmigrate protocol):

- `make gen-proto` — only `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}` and the proto changed (`git status --short`).
- `go build ./... && go vet ./...` — clean (run after every step below).
- `go test ./services/procurement/purchaseorder/ -count=1` — green: the
  domain suites (creation/validation from #35 plus the new exhaustive
  matrix — `TestLegalTransitions` walks all 10 edges asserting target
  status + exactly one event of the right type + `issued_at` on issue,
  mirrored against `transitionEvents`; `TestIllegalTransitions` walks the
  other 32 cells of 7×6 asserting the bare `ErrIllegalTransition`, unchanged
  status/issued_at, no event append; `TestCancelReceiptGuard` (receipts pin
  from every cancellable source, zero-receipt cancel passes, received →
  illegal-transition not has-receipts); `TestTerminalStatuses`;
  `TestEventLogSemantics`) and the store suites — the three #35 suites plus
  `TestStoreLifecycleHappyPath` (issue/acknowledge/short-close/normal-close/
  cancel-from-draft/cancel-from-issued, each with exactly one matching
  outbox row; the `po.issued` row's payload/entity/actor/version asserted),
  `TestStoreLifecycleGuards` (illegal transitions roll back fully — stored
  status unchanged, zero outbox rows; cancel-after-receipt → `ErrHasReceipts`
  with no write; cross-tenant → `ErrNotFound`; bad id → `ErrIDInvalid`),
  `TestStoreIssueConcurrency` (2 racers on one draft → exactly one winner,
  one `po.issued` row, stored status issued).
- `go test ./services/procurement/server/ -count=1` — green, including the
  new `TestPurchaseOrderGRPCLifecycle` (requester/viewer →
  PermissionDenied; bad id → 400 `po_id_invalid`; unknown → 404
  `po_not_found`; acknowledge-draft → 409 `po_illegal_transition` with no
  outbox row; issue with `x-trace-id` → issued + stamped issued_at + one
  `po.issued` row with actor/trace/payload; double issue → 409, still one
  row; acknowledge → one row; forced partial receipt → close → one row;
  cancel-terminal → 409; cancel after a forced receipt → 409
  `po_has_receipts` with the stored status unmoved and no row; plain cancel
  → one `po.cancelled` row; cross-tenant → 404; anonymous →
  Unauthenticated) and the widened `TestMapPurchaseOrderError`.
- `go test ./services/gateway/server/ -count=1` — green, including the new
  `TestPurchaseOrderLifecycleRoutesThroughServerNew` (all four routes wired
  via `WithProcurement`, buyer 200 with the updated PO JSON, path id
  forwarded, claims as gRPC metadata),
  `TestPurchaseOrderLifecycleRoutesRequirePermission`
  (requester/approver/finance/viewer → 403 on every action before any
  procurement call; admin passes), and
  `TestPurchaseOrderLifecycleRouteErrors` (po_illegal_transition /
  po_has_receipts → 409, po_not_found → 404, po_id_invalid → 400, machine
  codes + human sentences preserved).
- Affected packages sequential sweep:
  `go test ./services/procurement/purchaseorder/ ./services/procurement/server/
  ./services/gateway/server/ -count=1 -p 1` — all green in one run from a
  cleaned database (the dev-note 031 dbmigrate/leak protocol; the pre-existing
  `TestVendorGRPCList` L27 residue was wiped before and after, as in 035).
  The purchaseorder package was additionally run 8× consecutively — green
  every time — after the guard test's three cases were made order-independent
  (a fresh draft PO per case; the first draft iterated a Go map, so the
  cancel case sometimes ran first and the draft-status assertions read a
  cancelled PO — a test bug, not a store bug).
- `go vet ./... && go build ./...` re-run clean after all edits; `gofmt`
  applied to every touched Go file.
- pnpm build/lint/test not run by me (no TypeScript touched); the workflow
  script re-runs the full gate set after this dev note, per the ask.
- No compose or seed changes.

## Follow-ups

- **#37** (goods receipts): drive `MarkPartiallyReceived`/`MarkReceived`
  under the same PO row lock, advance `po_items.qty_received`, reject
  over-receipts, and emit `grn.posted`.
- Requisition-side conversion event (`pr.converted`/`pr.closed`) — the
  FR-11 gap dev-note 035 flagged; still open, candidate for #45.
- PO read endpoints (get/list) arrive with the PO list/detail issues; the
  transition responses are currently the only PO reads.
- Pre-existing, unchanged (dev-notes 029/030/031/035): the procurement
  server test cleanup leak, the `dbmigrate` advisory lock (hence `-p 1`),
  `go test -race` unrunnable locally (no cgo).
