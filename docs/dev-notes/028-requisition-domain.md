# 028 — Requisition domain model and state machine

- **Issue:** #28
- **Branch:** feature/28-28-requisition-domain
- **Date:** 2026-10-23

## What

- **`services/procurement/requisition`** — a pure domain package (no IO, following `vendor`): the `Requisition` aggregate, `Money` (integer cents + allowlisted currency) and `Item` value objects, the six-status state machine, per-transition domain events, and bare sentinel errors with machine codes (`requisition_illegal_transition`, `requisition_currency_invalid`, `requisition_qty_invalid`, …).
- **State machine** (maintainer decision 1, loose cancel): `draft → submitted`, `draft → cancelled`, `submitted → approved/rejected/cancelled`, `approved → cancelled/closed`; `rejected`/`cancelled`/`closed` terminal. Every legal transition appends exactly one event to the in-memory log (`Events()` copy view, `PullEvents()` drain for #45); a `requisition_created` event is recorded when a draft is created (needed by the #43 timeline). `Close()` exists now; #35 drives it via PO conversion.
- **Validation**: currency allowlist `{IDR, USD}` as domain constants (decision 3), item rules qty > 0, `unit_price_cents >= 0`, non-blank description. `Submit` re-checks the basket: empty → `requisition_items_empty`, invalid line → the item's error (both distinct from the 409 transition error). `UpdateItems` (draft-only, for #29's edit API) validates and recomputes `total_cents`; it records no event — it is not a transition.
- **Migration `0003_requisitions.sql`**: `procurement.requisitions` (status CHECK over the six states, currency CHECK `{IDR, USD}`, `UNIQUE (tenant_id, number)`, nullable `submitted_at`, vendor-shaped audit columns) + `procurement.requisition_items` (FK → requisitions, `qty CHECK (> 0)`); RLS ENABLE+FORCE + `tenant_isolation` + grants on both, 0001 pattern. Auto-applied via the embed FS (`migrations.go`) by `dbmigrate.Apply`.
- **409 contract as a pure mapping** (maintainer decision 2): `mapRequisitionError` in `services/procurement/server` maps `ErrIllegalTransition` → `codes.Aborted` (validation sentinels → `InvalidArgument`), proven by a DB-free unit test. The gateway's `Aborted` → HTTP 409 step lands with #29's endpoints (its `vendorError` switch currently has no `Aborted` arm).
- Docs: `docs/schema/procurement.md` gains the 0003 tables + state machine/error-code sections.

## Decisions

- **Migration is `0003_`, not `0002_`**: the plan named `0002_requisitions.sql`, but `0002_vendor_list_indexes.sql` already exists (#27) and `dbmigrate` rejects duplicate versions; same content, next version.
- **Items carry no `tenant_id`** (PRD §13 shape): their RLS policy derives tenancy from the parent requisition via `EXISTS`, so isolation stays transitive without denormalizing. Verified live (see below).
- **Domain errors are bare sentinels** (never wrapped by this package), so `err.Error()` carries the machine code verbatim through the gRPC message — same contract as `vendor`; `errors.Is` still works when callers wrap with `%w` (unit-tested).
- **Submit's failure order**: basket validation only applies from `draft`, so a non-draft submit reports `requisition_illegal_transition` (409) while a draft basket problem reports a 400-class validation code — the two contracts don't shadow each other.
- `approved → closed` is domain-owned from day one (edge in the map, `Close()` method, tested), even though nothing calls it until #35.
- Currency matching is exact (`IDR`/`USD`); case normalization is the API layer's job in #29.

## Evidence

- `go test ./services/procurement/requisition/...` — green: the full transition matrix as table-driven subtests, **7 legal edges** (each asserts new status + exactly one appended event of the right type) and **23 illegal cells** (6 statuses × 5 actions − 7 legal, including self-loops and terminal-state moves: each asserts `ErrIllegalTransition`, unchanged status/total/submitted_at, and no event appended), plus creation/validation failure paths (qty ≤ 0, currency outside allowlist, empty items at submit, blank description, negative price) and `PullEvents`/copy semantics.
- `go test ./services/procurement/server/...` — `TestMapRequisitionError` green without a database (`ErrIllegalTransition` → `codes.Aborted` with machine code verbatim, wrapped sentinel still `Aborted`, unknown → `Internal`); vendor gRPC integration tests skip as before without `TEST_DATABASE_URL`.
- **Migration verified against a live PostgreSQL 17** (scratch database + throwaway roles on the local instance, dropped afterwards): all three migrations apply in order and re-apply is a no-op; bad status/currency/qty/price rejected by the named CHECK constraints; `UNIQUE (tenant_id, number)` enforced, same number OK in a second tenant; RLS `FORCE`d + `tenant_isolation` on both tables; under `SET ROLE eproc_app` tenant B sees 0 of tenant A's requisitions/items and cannot update, delete, or smuggle-insert items under A's requisition.
- `go vet ./... && go build ./...` clean; `gofmt` clean on touched files; `docker compose config` valid (full gate set re-run by the workflow script before push).

## Follow-ups

- #29: store/tenant-scoped queries + number generation, create/edit/submit gRPC + REST endpoints, gateway `Aborted` → 409 arm (pattern: `vendorError`, `services/gateway/server/vendors.go`).
- #30/#31: the approval engine calls `Approve`/`Reject`; #35: PO conversion calls `Close` and `vendor.GetForUse`.
- #42/#45: event store + outbox drain via `PullEvents()`; the event payload (ids, totals) gets enriched where events are serialized, not in the domain.
