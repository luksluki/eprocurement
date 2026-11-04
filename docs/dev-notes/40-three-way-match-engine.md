# Dev note — #40 Three-way match engine with tolerance

- **Issue:** #40
- **Branch:** feature/40-three-way-match-engine
- **Date:** 2026-10-28

> **Provenance:** the maintainer answered the four open questions on
> 2026-10-28 (issue #40 comments) and an agent decision resolved the one
> fork they left implicit:
>
> 1. **Re-match policy:** `matched` rejects a re-run (409
>    `inv_already_matched`); `pending`/`discrepancy` MAY be re-matched
>    (resolution after a late GRN; each run appends its own event —
>    history stays append-only).
> 2. **Qty comparison basis:** cumulative — the PO's invoiced total across
>    ALL invoices vs per-line `qty_received` (the #37/#39 cumulative
>    philosophy; only that prevents two individually-reasonable invoices
>    jointly over-billing).
> 3. **Tolerance surface:** per-tenant table + `Get`/`Replace` gRPC +
>    admin-only REST `GET|PUT /api/v1/match-settings`, with the new RBAC
>    entry `match:configure` (the `approval:configure` twin).
> 4. **Attachment gate:** #40 does NOT gate matching on a ready attachment
>    and introduces no synchronous procurement→documents call (resolves
>    the 'and/or' the 2026-10-27 #39 decision left open; the obligation
>    stays with the demo flow/UI #41).
> 5. **(Agent, 2026-10-28, maintainer unavailable — one-sentence override
>    welcome in review):** qty tolerance is **upper-side only** —
>    cumulative invoiced exceeding `qty_received × (1+qty_pct%)` is a
>    discrepancy; under-invoicing (progress billing) is NOT. Basis: the
>    maintainer's own 'matched stays stable' rationale (invoiced fixed,
>    received monotonically grows) only holds for a one-sided upper check,
>    and the cumulative rationale was framed entirely around 'exceeding
>    receipts'. Price stays symmetric ±%.

## What changed

- **Migration `0010_match_settings.sql`** — `procurement.match_settings`
  (`tenant_id` PK, `qty_pct`/`price_pct` integer CHECK 0..100, audit
  columns), RLS + grants on the 0005 approval_rules pattern. One row per
  tenant (the tolerance pair is a scalar, not a rule set); **absence means
  the effective defaults 0%/0%** — strict, never an error, no seeding step
  for a fresh tenant.
- **`services/procurement/match` (new)** — the engine (`match.go`) is
  **pure** (no IO, no clock): integer cross-multiplication only. Per
  invoice line: qty guard `invoiced×100 > received×(100+pct)` (upper side
  only, equality within, `received=0 ∧ invoiced>0` always a breach) and
  price guard `|Δ|×100 > po_price×pct` (symmetric; `po_price=0` matches
  only a zero invoice price). Result lines follow the PO items'
  `(created_at, id)` order — the store's load order — regardless of the
  invoice's line order; `ResultJSON()` marshals a fixed struct so
  identical state yields byte-identical JSON. `ValidateSettings` bounds
  0..100 (`match_qty_pct_invalid` / `match_price_pct_invalid`). The store
  (`store.go`): `Match` runs the pinned step order in one tenant
  transaction — non-UUID fail-fast → unlocked header resolve (cross-tenant
  is `inv_not_found` via RLS) → **PO row FOR UPDATE**
  (`purchaseorder.LoadForUpdate`, the shared serialization point —
  concurrent matches serialize here and the cumulative SUMs read one
  snapshot) → invoice re-read `FOR UPDATE` + lines → re-run guard
  (`invoice.ErrAlreadyMatched` from `matched`; pending/discrepancy pass) →
  tolerances (defaults 0/0) → cumulative `SUM(il.qty)` per line over every
  invoice of the PO (this one included) → the engine → guarded
  `UPDATE … WHERE status IN ('pending','discrepancy')` (0 rows → the same
  409, belt and braces) → **exactly one outbox row** — `invoice.matched`
  or `invoice.discrepancy`, `entity_type=invoice` (§14.6; the grn.posted
  pattern, ADR-0001) — committed with status + `match_result`.
  `GetSettings`/`ReplaceSettings` (upsert, validated) complete the
  tolerance surface. `purchase_orders` is never written.
- **`services/procurement/invoice`** — `ErrAlreadyMatched`
  (`inv_already_matched`) + `MatchGuard(status)`: matched is final,
  pending/discrepancy may be (re-)matched. The #39 file otherwise
  untouched.
- **proto + gen** — additive `MatchInvoice` / `GetMatchSettings` /
  `ReplaceMatchSettings` RPCs with `MatchInvoiceRequest` /
  `MatchSettings` / settings request messages. `make gen-proto`
  regenerated only `gen/go/procurement/v1/*` (the documents gen churn was
  line-endings only and reverted).
- **procurement server** — `api` gains the `match.Store`;
  `methodPermissions` maps `MatchInvoice` → `rbac.InvoiceWrite`
  (finance/admin — the finance reviewer persona; **no new permission for
  the act of matching**) and the two settings RPCs → `rbac.MatchConfigure`
  (admin-only). `mapInvoiceError` gains the `inv_already_matched` →
  Aborted arm; `mapMatchError` covers the settings codes; `main.go`
  constructs the store.
- **rbac** — `MatchConfigure = "match:configure"` with the admin-only
  matrix entry (`nil` roles — the `approval:configure` twin) and the
  `wantPermission` expectations row.
- **gateway** — `ProcurementClient` widened (compile-forced stubs joined
  `fakeRequisitionClient`); `invoices.go` adds
  `POST /:id/match` behind `RequirePermission(rbac.InvoiceWrite)` and the
  `inv_already_matched` message; new `matchsettings.go`: admin-only
  `GET|PUT /api/v1/match-settings` behind `rbac.MatchConfigure` with body
  `{"qty_pct": n, "price_pct": n}`; `server.go` registers the routes under
  `WithProcurement`.
- **Worker: untouched** — `documents.history` binds `#` and
  `invoice.discrepancy` is already in `NotificationsRoutingKeys`
  (services/worker/topology/topology.go:54-60,86).
- **Docs** — `docs/schema/procurement.md` gains the "Three-way match
  (#40)" section (table, pinned engine semantics, the `match_result`
  shape with the reason-line formats, store contract, settings, no
  attachment gate, API + error codes) and the follow-ups entry;
  `docs/rbac.md` gains the `match:configure` matrix row and the #40
  landed entry.

## Decisions

- **One result line per *invoiced* PO line, ordered by the PO items** —
  the engine checks "tiap baris invoice" (AC): lines this invoice bills,
  in the PO items' `(created_at, id)` order. Unbilled PO lines have no
  verdict line (nothing to explain), while the cumulative qty sum still
  counts every invoice of the PO.
- **Reason lines in the `inv_over_bill` family, pinned** —
  `inv_over_bill_qty: po_item=<id> invoiced=<i> received=<r> pct=<p>` and
  `inv_over_bill_price: po_item=<id> po_price=<p> invoice_price=<q>
  pct=<p>` — machine code before the first colon, then numbers (the
  grn_over_receipt / inv_over_bill format family), `""` on a clean line.
- **`match_result` is jsonb → PostgreSQL stores its canonical form.** The
  engine's `ResultJSON` is byte-stable pre-storage; the column's
  canonical text is deterministic too (same logical JSON → same bytes), so
  identical state still round-trips byte-identically — the store test
  compares an unchanged re-run's stored bytes exactly and compares
  returned-vs-stored semantically.
- **Outbox payload = identity + verdict** — invoice id/number, PO number,
  landed status, the same `ResultLine` slice `match_result` stores, and
  `matched_by`; entity_type `invoice` (the timeline key), matching the
  grn.posted payload style.
- **Lock order PO → invoice** — the PO lock is the shared serialization
  point (same as create/receipt), so two matches of one invoice AND two
  matches of sibling invoices of one PO both serialize; the invoice
  `FOR UPDATE` re-read after the PO lock makes the status guard race-free.
- **Settings stored per tenant with effective defaults, not seeded** —
  `GetMatchSettings` answers 0/0 when the row is absent (RLS-scoped, so
  another tenant's row is invisible, not readable-as-zero-with-leak).
- **`match:configure` denies finance too** — admin-only exactly like
  `approval:configure` (the maintainer's twin framing); finance keeps the
  match *action* through `invoice:write`.

## Evidence

Against the local compose PostgreSQL
(`TEST_DATABASE_URL=postgres://eprocurement:eprocurement@localhost:55432/eprocurement?sslmode=disable`;
packages run sequentially `-p 1` per the dev-note 031 protocol):

- `make gen-proto` — only `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}` and the proto changed; the documents gen files
  showed line-ending-only churn and were reverted
  (`git diff --numstat` empty).
- `go build ./... && go vet ./...` — clean (re-run after every step;
  final run after the full redo: `GATE_BUILD_VET_OK`).
- `gofmt -l` on every touched file — clean.
- `go test ./services/procurement/match/ -count=1` (no DB env) — green:
  `TestValidateSettings`, `TestRun` (13 table cases: full receipt, partial
  receipt, progress billing 30/60, received=0 breach, cumulative 80 vs 60
  at 0%/33%/34%, qty boundary 55 = 50×1.10 within, price ±15% breaches,
  price boundary 1100 = 1000×1.10 within, po_price 0 both arms, one
  breach flags the whole invoice, unbilled line), `TestRunDeterministicJSON`
  (shuffled input lines → identical bytes; re-run → identical bytes; the
  pinned JSON shape verbatim), `TestRunUnknownPOItem`,
  `TestBreachBoundaryArithmetic` (8 cross-multiplication boundaries incl.
  a 2×10⁹ no-overflow row), `TestDefaultSettings`.
- `go test ./services/procurement/match/ -count=1` (with DB) — green:
  `TestStoreMatchHappyPath` (pending→matched, pinned `match_result`
  string, exactly one `invoice.matched` row with `entity_type=invoice`,
  `purchase_orders` status/updated_at/updated_by byte-identical, re-run →
  `inv_already_matched` with no extra row),
  `TestStoreMatchDiscrepancyAndRerun` (received=0 → discrepancy with the
  pinned reason; unchanged re-run stores byte-identical bytes and appends
  its own event; late GRN → matched on the third run — three events in
  order discrepancy/discrepancy/matched; matched then final),
  `TestStoreMatchCumulativeBasisAndSettings` (first 40-invoice matches
  alone — progress billing; the second flags at cumulative 80 vs received
  60; tenant tolerance 34% resolves it on a re-run),
  `TestStoreMatchPriceSymmetric` (+15% breach / exactly +10% within /
  −15% breach across three POs), `TestStoreMatchGuards` (bad id
  `inv_id_invalid`, missing and cross-tenant `inv_not_found`
  indistinguishable, nothing written), `TestStoreMatchConcurrency`
  (two parallel matches → 1 commit + 1 `inv_already_matched`, exactly one
  event), `TestStoreSettings` (defaults 0/0, both bound codes, upsert,
  RLS isolation between tenants A and B).
- `go test ./services/procurement/invoice/ -count=1` — green incl. the new
  `TestMatchGuard` (matched → sentinel, pending/discrepancy pass, the
  sentinel string verbatim).
- `go test ./services/procurement/server/ -count=1` — green incl.
  `TestMatchGRPC` (requester/buyer/approver/viewer denied on MatchInvoice
  at the interceptor; bad id 400 / missing 404 machine codes; no-receipt
  match → discrepancy with `inv_over_bill_qty` and `"qty_received":0` in
  `match_result_json`, one event; late GRN via the buyer → finance re-run
  → matched, two events; re-run on matched → Aborted
  `inv_already_matched`, still two; cross-tenant 404; settings denied for
  finance/buyer/viewer; admin default 0/0, both bound codes, replace
  5/2 + read-back; anonymous Unauthenticated on both surfaces) and
  `TestMapMatchError`; `TestMapInvoiceError` gained the already-matched
  arm.
- `go test ./internal/platform/rbac/ -count=1` — green
  (`TestMatrixCoversEveryPermission`, `TestEveryRoleAgainstMatrix` with
  `MatchConfigure: {}`).
- `go test ./services/gateway/server/ -count=1` — green:
  `TestMatchInvoiceRoute` (buyer/viewer 403 with `required:
  invoice:write` BEFORE any procurement call; finance 200 with the matched
  body; id forwarded; claims as gRPC metadata),
  `TestMatchInvoiceRouteErrors` (409 `inv_already_matched`, 404
  `inv_not_found`, 400 `inv_id_invalid`), `TestMatchSettingsRoutes`
  (finance/buyer/viewer 403 with `required: match:configure` before any
  call on BOTH verbs; admin GET 0/0 defaults; PUT forwards and echoes
  5/2; metadata propagated), `TestMatchSettingsRouteErrors` (400
  `match_qty_pct_invalid` / `match_price_pct_invalid` through the shared
  mapping; malformed body 400 `body_invalid` before any call).
- Sequential sweep of all touched packages:
  `go test ./internal/platform/rbac/ ./services/procurement/match/
  ./services/procurement/invoice/ ./services/procurement/server/
  ./services/gateway/server/ -count=1 -p 1` — all five `ok` from a
  cleaned database.
- `docker compose exec postgres psql … \d procurement.match_settings` —
  0010 applied: PK `tenant_id`, both CHECKs, forced RLS `tenant_isolation`
  policy, `match_settings` present in `procurement.schema_migrations`.
- **Known pre-existing flake, verified NOT mine:** an immediate re-run of
  `go test ./services/procurement/server/` fails
  `TestVendorGRPCList` with `vendor_duplicate_code` on `L27-A` — the
  cleanup leak dev-note 39 already documents. Reproduced identically on a
  pristine `HEAD` worktree (first run `ok`, second run `FAIL
  TestVendorGRPCList`), so #40 neither introduces nor worsens it; the
  sweep protocol stays "clean the tenants between runs".
- pnpm build/lint/test not run by me (no TypeScript touched — the same
  justification as dev-notes 037/039); the workflow script re-runs the
  full gate set after this dev note, per the ask.
- No compose/seed/Makefile changes.

## Follow-ups

- **#41 (invoice list + UI):** the list endpoint, the `/app/invoices`
  pages (rendering `match_result_json`), and the demo-flow enforcement of
  "attachment required" — deliberately not gated here (provenance #4).
- Pre-existing, unchanged (dev-notes 029–039): the procurement server
  test cleanup leak (L27/G-01 residue — verified against HEAD in this
  note's evidence), the `dbmigrate` advisory lock,
  `go test -race` unrunnable locally (no cgo).
- A tolerance-history view (which tolerance a past run used) would need
  either a settings snapshot inside `match_result` or an audit table —
  the current payload records the verdict, not the thresholds; fine for
  the demo unless finance asks.
