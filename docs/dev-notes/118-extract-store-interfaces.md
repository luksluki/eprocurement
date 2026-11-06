# Dev note — #118 Per-domain store interfaces for DB-free procurement API tests

- **Issue:** #118 (`area: procurement`, `type: chore`, `size: L`)
- **Branch:** feature/118-extract-store-interfaces
- **Date:** 2026-11-06

> **Provenance:** #118 was spun out of #57 by the owner's recorded decision 4
> on #57 — "The api-layer refactor (interface extraction for 8+ concrete
> stores) is OUT of scope for #57 (issue size M); a separate issue is being
> opened for it." (read via `gh issue view 57 --comments` this session; the
> decision is recorded with "maintainer override welcome in PR review").
> #118 itself has 0 comments (`gh issue view 118`). The issue's draft AC
> names 8 domains; the current wiring takes 9 store parameters (`export`
> joined with #53), so all nine are extracted. No architecture, dependency,
> CI, Makefile, compose, or seed change: the interfaces are consumer-side
> declarations inside the existing `server` package, and the fakes use only
> modules already in `go.mod` (`github.com/google/uuid`, `pgx/v5`).

## What changed

- **`services/procurement/server/stores.go`** (new) — nine consumer-side
  interfaces, one per domain, holding exactly the method surface `api.go`
  calls: `VendorStore` (5 methods), `RequisitionStore` (5), `ApprovalStore`
  (5), `PurchaseOrderStore` (7), `GoodsReceiptStore` (1), `InvoiceStore` (3),
  `MatchStore` (3), `DashboardStore` (1), `ExportStore` (1) — 31 methods,
  equal to the 31 distinct store calls in `api.go`. Every signature is
  copied verbatim from the concrete `*Store`, including
  `RequisitionStore.Submit`'s `materialize func(tx pgx.Tx, r
  requisition.Requisition) error` callback and
  `ApprovalStore.MaterializeTx(ctx, tx pgx.Tx, r)`. A `var (...)` block of
  compile-time assertions (`_ VendorStore = (*vendor.Store)(nil)`, …) proves
  every concrete store satisfies its interface unchanged.
- **`services/procurement/server/api.go`** — the nine field types of the
  `api` struct are now the interfaces; the now-unused `export` import is
  dropped. No handler body changed (the diff is the struct block plus one
  import line).
- **`services/procurement/server/server.go`** — `New`'s nine store
  parameters take the interfaces (doc comment added); the nine domain
  imports it no longer needs are dropped. `methodPermissions` untouched.
  `services/procurement/cmd/procurement/main.go:109` is unchanged and
  compiles as-is (the concrete stores satisfy the interfaces).
- **`services/procurement/server/fakes_test.go`** (new) — one shared
  `memState` plays the database: every collection is keyed by tenant first
  (the RLS twin — a cross-tenant id reads as missing), plus the per-tenant
  number counters and the submit/decide idempotency-key ledgers. Nine fakes
  over it implement the interfaces by calling the **same pure domain
  functions the concrete stores call** (`requisition.New`/`Submit`/
  `UpdateItems`, `approval.ValidateRuleSet`/`Materialize`/`Decide`,
  `purchaseorder.New` + lifecycle methods + `ApplyReceipt`,
  `goodsreceipt.Validate`, the invoice validators + `OverBillError` +
  `MatchGuard`, `match.ValidateSettings`/`Run`), so the sentinels reaching
  the gRPC mappers are the real ones; only persistence is faked. Each fake
  records the tenant ids it receives and supports an injected `err` (the IO
  path). Also `fakePinger`, `unitCtx` (incoming metadata), and seeding
  helpers (`seedApprovedReq`, `seedAcknowledgedPO`, `seedInvoiceRaw`).
- **`services/procurement/server/api_unit_test.go`** (new) — 14 DB-free
  tests against the `api` struct directly:
  - `TestUnitUnauthenticatedGate` — all 29 RPCs answer `Unauthenticated`
    without caller metadata, and no fake is touched.
  - `TestUnitVendorValidationAndMapping` — bad code/name/email →
    `InvalidArgument` machine codes before any store call; normalization +
    audit on create; `auth.TenantID` forwarded; duplicate →
    `AlreadyExists vendor_duplicate_code`; unknown → `NotFound`;
    deactivated → `FailedPrecondition`.
  - `TestUnitVendorIOErrorIsInternal` — an injected store error →
    `Internal "internal error"` (no leak).
  - `TestUnitRequisitionValidation` — currency (missing/EUR), qty,
    negative price, blank description → machine codes; `" idr "` → `IDR`,
    server-side total, `PR-000001`; non-UUID id → `requisition_id_invalid`.
  - `TestUnitListFilterValidation` — unknown status / absurd page on the
    vendor, requisition, PO, and invoice lists → the four domains' codes,
    store `List` never called.
  - `TestUnitServiceLayerBounds` — 65-char submit and decide idempotency
    keys, 1001-char decide reason, 1001-char GRN notes → `InvalidArgument`
    before any store call.
  - `TestUnitMatchToleranceBounds` — `-1` / `101` → `match_qty_pct_invalid`
    / `match_price_pct_invalid`; defaults 0/0; replace round-trip.
  - `TestUnitTenantScoping` — tenant B reading tenant A's vendor and
    writing tenant A's requisition (edit, submit), conversion, GRN, invoice,
    and match → `NotFound` with the RLS-equivalent codes; tenant B's lists
    see nothing; every touched fake recorded the caller's tenant id; tenant
    B may reuse tenant A's vendor code.
  - `TestUnitOverReceiptTypedError` / `TestUnitOverBillTypedError` — the
    typed errors reach `Aborted` with the pinned detail lines verbatim;
    nothing lands; unknown receipt line → `grn_po_item_unknown`; duplicate
    invoice number → `AlreadyExists inv_number_duplicate`.
  - `TestUnitSubmitFlow` — no-rules submit → `FailedPrecondition
    approval_rules_missing` with the key unrecorded; empty basket;
    rule normalization; ownership (`requisition_forbidden`, admin
    exception); keyed submit with steps in-band, replay, key conflict;
    immutability; self-approval and role bans; approve → approved;
    conversion (`created=true`, closed requisition), replay
    (`created=false`, vendor ignored), not-approved and deactivated-vendor
    gates.
  - `TestUnitPOLifecycleAndMatchFlow` — lifecycle machine codes, cancel
    edges and the receipts guard (`po_has_receipts`), partial receipt,
    match discrepancy → late GRN → matched → `inv_already_matched`,
    short-close, and the cumulative over-bill line.
  - `TestUnitReadsDashboardExport` — header-only list rows (requisition,
    PO, invoice incl. no `match_result`), detail reads with bodies, the
    three dashboard widgets and tenant B's empty snapshot, and per-tenant
    export event ids.
  - `TestUnitNewAcceptsFakes` — `New(log, fakePinger{}, <nine fakes>)`
    builds and registers the server with no database.
  - Compile-time assertions that the nine fakes satisfy the interfaces.

## Decisions

1. **Consumer-side interfaces in `package server`**, one per domain (Go's
   "accept interfaces where they are used"; the hexagonal port sits at the
   adapter that consumes it). The domain packages and their stores are not
   touched, there is no import cycle, and the fakes stay small. A single
   aggregate interface was rejected: it would couple unrelated domains and
   force every fake to implement 31 methods.
2. **Exactly the used surface (31 methods).** Methods the API never calls
   (e.g. `vendor.Store.GetForUse`, the tx-scoped `ForUseTx` the PO
   conversion uses internally) are not part of the ports.
3. **`pgx.Tx` stays in `Submit`/`MaterializeTx` verbatim** (AC: no store
   signature change). The `server` package already imported `pgx` for the
   submit closure in `api.go`; a tx-free port would require changing store
   signatures and is out of scope. The fake submit passes a nil tx to the
   callback — both fakes own all the state.
4. **Fakes over mocks** (the dev-note 057 convention): stateful in-memory
   fakes reusing the pure domain functions, no mocking library (no new
   dependency). The fakes mirror the store contracts the handlers depend
   on: tenant-first keying (cross-tenant = not found), uuid fail-fast
   codes, submit/decide replay + conflict, copy-on-write commit (a failed
   materialize leaves the draft and the key unrecorded), number/id
   allocation only after validation (the real transaction rolls the
   counter back), header-only list rows, and the list orderings.
5. **Unit tests call the `api` struct directly** with *incoming* metadata —
   `rbac.FromMetadata` reads `metadata.FromIncomingContext`
   (internal/platform/rbac/grpc.go:86-87), so the integration suite's
   outgoing-metadata `callerCtx` does not apply. No listener, no
   interceptor: interceptor-level `PermissionDenied` stays covered by the
   integration suite, while the handler-level ownership denial
   (`requireOwner`) is unit-covered.
6. **Integration suite untouched.** `api_test.go` is byte-identical to HEAD
   and keeps exercising the concrete stores through the same interfaces,
   so the cross-tenant denial cases remain there (none removed) and are now
   also in the unit suite.
7. **Pre-existing gofmt drift left alone:** `api.go`'s `dashboardPB` literal
   (around lines 1020-1027) is not gofmt-aligned at HEAD `88229a4` already
   (identical `gofmt -d` before/after). Unrelated to #118 (golden rule 2);
   CI runs no gofmt step.

## Evidence

All commands run from the repo root on 2026-11-06 (Windows/Git Bash; compose
stack healthy per `docker compose ps`). `TEST_DATABASE_URL` was derived the
way `Makefile:84` does it (`DATABASE_URL` from `.env`). Unit-only runs unset
every `TEST_*` variable (`env -u TEST_DATABASE_URL -u TEST_MONGO_URI …`).

- **Baselines (before any code change):**
  - `server` package, unit-only: `go test ./services/procurement/server/
    -count=1 -coverprofile=… -coverpkg=./services/procurement/server` →
    **8.8%**.
  - #57 parity set (the 11-package coverpkg list of `make
    cover-procurement`), unit-only → **23.1%** (Makefile:116-117 says
    "~22%").
  - Same parity set **with the database**, the #57 gate's command
    (`-skip 'Scale100k'`), run on HEAD `88229a4` in a temporary detached
    worktree (removed afterwards) → **88.0%**.
- **After** (final tree):
  - `server` package, unit-only → **93.3%** (`ok … 0.181s`); `-v` shows 19
    PASS (14 `TestUnit*` + 5 `TestMap*`) and the 20 `startProcurement`
    integration tests SKIP — the new suite needs no `TEST_DATABASE_URL`.
  - Parity set, unit-only → **41.6%**.
  - `make cover-procurement` (database) → `total: 89.8%` +
    `OK: procurement coverage 89.8% meets the 80% threshold`; all 11
    packages `ok`, i.e. the DB-backed `store_test`/`store_list_test` suites
    pass unchanged (`Scale100k` skipped by the target's design).
- **Unit suite speed and determinism:** `go test
  ./services/procurement/server/ -run TestUnit -count=1` → `ok 0.151s`;
  `-count=20` → `ok 0.171s` (20 runs over Go's randomized map order).
- **Integration suite against PostgreSQL:** `go test
  ./services/procurement/server/ -count=1 -v` with `TEST_DATABASE_URL` →
  39 PASS, 0 FAIL, 0 SKIP: all 25 `api_test.go` tests (20 gRPC tests via
  `startProcurement` + 5 `TestMap*` mappers) alongside the 14 `TestUnit*`;
  `ok … 2.368s`.
- **#57 unit-suite duration gate:** `bash scripts/dev/test-unit.sh` →
  `unit suite took 5s (budget 120s)`, `PASS`, exit 0 (an earlier run in
  this session, colder build cache, took 16s).
- `go vet ./...` → exit 0; `go build ./...` → exit 0.
- **Surface check:** the distinct `a.<store>.<Method>` calls grepped from
  `api.go` = 31 = the interface methods in `stores.go`.
- `git diff --quiet HEAD -- services/procurement/server/api_test.go
  services/procurement/cmd/procurement/main.go Makefile .github
  compose.yaml scripts go.mod go.sum` → unchanged.
- gofmt: the three new files are clean; `api.go`/`server.go` are listed by
  `gofmt -l` only because the working copies are CRLF (`git ls-files
  --eol` → `i/lf w/crlf`, the repo-wide state noted in dev-note 115) plus
  the pre-existing `dashboardPB` alignment (Decision 7).
- **Not run here:** `-race` (`CGO_ENABLED=1` fails: no `gcc` on this
  machine); the pnpm gates (no TypeScript touched — the workflow script
  runs them); full `go test ./...` with the integration environment (the
  workflow script's gate).

## Follow-ups

- **Stale Makefile comment:** `Makefile:113-117` says the parity set reads
  "~22%" unit-only; it now reads 41.6%. Update it in an issue allowed to
  touch the Makefile (this one is not).
- **Unit-only coverage guard:** with the API layer at 93.3% unit-only, the
  `server` package could get its own DB-free coverage guard, or the #57
  gate could depend less on the database — a CI change, maintainer call.
- **`api.go` `dashboardPB` gofmt drift** (pre-existing, Decision 7) — a
  one-line `gofmt -w` in any later PR touching that function.
- **Same pattern for other services** (`identity`, `documents` servers) if
  their API layers should become DB-free testable too.
- **Fake fidelity limits** (by design): no transactions, row locks, or
  PostgreSQL cast errors — concurrency/serialization and SQL behavior stay
  the integration suite's job.
