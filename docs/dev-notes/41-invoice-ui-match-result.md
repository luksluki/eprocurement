# Dev note — #41 Invoice UI with match result

- **Issue:** #41
- **Branch:** feature/41-invoice-ui-match-result
- **Date:** 2026-10-29

> **Provenance:** the maintainer answered the three open questions on
> 2026-10-29 (issue #41 comments), so no agent-taken decision needed the
> 035/036/037 protocol:
>
> 1. **Attachment-required enforcement = hard gate in the UI.** The Match
>    button stays disabled until the invoice has a vendor attachment in
>    `ready` state — client-side only; the server stays non-checking per
>    #40's decision (#39 provenance: "UI #41 requires it").
> 2. **Performance evidence = migration index + EXPLAIN (ANALYZE, BUFFERS)
>    in the dev note now; the 100k fixture + p95 budget is deferred to #56**
>    (the dedicated performance-budget issue) — avoiding the CI fixture
>    contention documented in PR #107.
> 3. **Create-invoice form = new page `/app/invoices/new?po=<id>`** with a
>    "Create invoice" link (finance role) from the #38 PO detail page; the
>    invoice flow stays under `/app/invoices` per PRD §11.

## What changed

- **Proto + gen** — additive `ListInvoices` RPC with
  `ListInvoices{Request,Response}` (the #38 list envelope shape; header-only
  rows documented in the proto comments: items and `match_result_json`
  always empty on list rows). `make gen-proto` regenerated only the
  procurement gen files (the documents gen churn was line-endings only and
  reverted).
- **`services/procurement/invoice`** — the #41 filter model in the domain
  (`ListFilter`/`ValidateList`: the three-status whitelist
  pending|matched|discrepancy, page/page_size clamping 1/20/max 100 with the
  `MaxPage` bound, query trimmed + uppercased with escaped LIKE wildcards;
  sentinels `inv_status_invalid` / `inv_page_invalid`) and the store read
  `List` (#32's query shape verbatim: page subquery + exact COUNT as an
  uncorrelated scalar subquery / InitPlan + past-the-end COUNT fallback),
  ordered `created_at DESC, id DESC`, rows header-only (no lines, no
  match_result).
- **Migration `0012_invoice_list_indexes.sql`** —
  `invoices_tenant_status_idx (tenant_id, status)`, direct parity with 0011
  (POs) and 0003 (requisitions). `migrations.go` needed no change.
- **procurement server** — `ListInvoices` mapped to `rbac.InvoiceRead` in
  `methodPermissions`; thin `api.go` handler; `mapInvoiceError` gains the two
  list-code arms (InvalidArgument).
- **gateway** — `ProcurementClient` widened with `ListInvoices` (the
  compile-forced stub joined `fakeRequisitionClient`);
  `GET /api/v1/invoices` behind `RequirePermission(rbac.InvoiceRead)` with
  the envelope `{invoices, total, page, page_size}`; `invoiceMessages` gains
  the `inv_status_invalid` / `inv_page_invalid` human sentences.
- **web `/app/invoices` (list)** — replaces the PlaceholderPage: the #38
  PO-list pattern (server component on DataTable/Badge/PageHeader/
  TablePagination/EmptyState/ErrorState; q/status/page_size filters in the
  URL through a plain GET `<form>`, no client JS; number column links to the
  detail; status badges pending=neutral/matched=success/discrepancy=danger;
  vendor_id in mono; money-formatted totals; created_at).
- **web `/app/invoices/[id]` (detail)** — header with the link to the billed
  PO, the lines table (po_item_id, qty, unit price, line total), the
  attachments panel, and the **MatchResultPanel** rendering the #40
  `match_result_json`: the verdict badge, one row per invoiced PO line with
  ordered/received/invoiced + Δ qty (= invoiced − received, the cumulative
  guard's comparison basis), PO vs invoice unit price + Δ (signed, money),
  breach badges, and the pinned reason line localized with its parsed
  numbers — the mismatch cause on one screen (the issue's AC).
- **`InvoiceActions` (Match button)** — renders only for invoice:write
  holders on a pending/discrepancy invoice and stays **disabled with its
  reason until an attachment is `ready`** (the hard gate); success runs
  `router.refresh()`; 409 `inv_already_matched` maps inline; 401 runs the
  refresh dance.
- **`InvoiceAttachments`** — the embedded list (filename / size / status /
  download via the BFF-minted presigned URL) plus the finance upload: the
  #33 flow verbatim (declare through the BFF → PUT straight to the MinIO
  presigned URL → complete verifies server-side); a completed upload
  refreshes the panel, and its `ready` state unlocks the Match button.
- **`lib/invoice-eligibility.ts`** — the #38 po-eligibility pattern:
  `holdsInvoiceWrite` (finance/admin), `canMatchInvoice` (pending/
  discrepancy — the #40 re-run guard), `canCreateInvoice` (the #39 PO-status
  set), `hasReadyAttachment` (the hard gate's condition).
- **`lib/match-result.ts`** — pure parsing of the pinned #40 JSON (typed
  guard per line, null on malformed), the Δ derivations, `parseReason` for
  the two pinned reason formats, and the `fill` template helper.
- **`lib/invoice-filters.ts`** — the #38 URL filter model over the three
  invoice statuses.
- **`/app/invoices/new?po=<id>` + `InvoiceForm`** — the create page
  (maintainer decision 3): the server component loads the PO once (item ids
  + prefill prices + the eligibility gate), the client form mirrors the #39
  validation client-side (number trim ≤ 100, qty integer > 0, price ≥ 0,
  ≥ 1 filled line; lines without a qty are not billed), prefills every unit
  price with the PO price, redirects to the new invoice's detail on
  success, parses the pinned 409 `inv_over_bill` line so the exact amounts
  render inline (money-formatted), and runs the refresh dance on 401. The
  #38 PO detail gains the "Create invoice" link for finance on an
  invoicable PO (`canCreateInvoice`).
- **BFF proxies** — `POST /api/invoices` (create; mirrors the #39 shape
  validation, forwards the pinned 409 verbatim),
  `POST /api/invoices/[id]/match`, `POST /api/invoices/[id]/attachments`
  (declare), `…/[attachmentId]/complete`, `…/[attachmentId]/download` (the
  #33 requisition route twins) — the access token stays server-side
  throughout.
- **i18n** — new `invoices` (list), `invoiceDetail` (detail incl. actions/
  attachments/match-panel copy with the reason and delta templates), and
  `invoiceForm` dictionary sections, ID + EN; `purchaseOrderDetail.actions`
  gains `createInvoice`.
- **Docs** — `docs/schema/procurement.md` (the "Invoice list" section with
  the 0012 index and the EXPLAIN evidence + the "Invoice UI with match
  result" section; follow-ups entry updated), `docs/rbac.md` (#41 landed
  entry).

## Decisions

- **List ordering is `created_at DESC, id DESC`, not number DESC** (the one
  deliberate deviation from the #38 twin): vendor invoice numbers are the
  issuer's free-form convention (mixed case, no sequence), so number order
  is not creation order; the unique id is the deterministic tie-breaker for
  stable pagination.
- **The prefix arm matches case-insensitively via
  `upper(number) LIKE upper(q) || '%'`** — the PO list could uppercase the
  query and compare against canonically uppercase stored numbers; vendor
  numbers have no canonical case, so both sides fold. Wildcards stay
  escaped (the shared `escapeLike`).
- **The hard gate lives in `InvoiceActions` as `disabled` + the reason
  sentence** (not by hiding the button): finance sees the action exists and
  what unlocks it — upload a vendor attachment to `ready` — which is the
  demo flow the maintainer's decision describes. The gate's condition
  (`hasReadyAttachment`) reads the attachments the detail read already
  embeds; no extra round trip.
- **The EXPLAIN fixture seeds rows through SQL under RLS bypass, not the
  domain**: 60k invoices in one tenant (10% matched) with `VACUUM (ANALYZE)`,
  EXPLAINed under the real RLS context, then deleted and vacuumed — the
  domain's cumulative over-billing guard would reject the bulk load by
  design, and the plan shape only needs the rows. The status-filter shape
  runs both arms on the new index (below, Evidence); the 100k fixture +
  p95 budget stays with #56 per decision 2.
- **Store/gRPC list tests set `matched`/`discrepancy` via SQL fixture
  updates** (a `setInvStatus` / `grpcSetInvoiceStatus` helper): only the
  #40 match writes those statuses, and pulling the match store into the
  invoice package's tests would invert the packages' dependency direction
  (match already depends on invoice). The list only reads the stored
  values.
- **The list's gateway fake returns a header-only row** (`sampleInvoice`
  with items nil) — procurement guarantees header-only rows; the renderer
  renders what it is given (the first draft asserted header-only-ness
  against an item-carrying fake and failed, correctly).
- **Δ price renders as signed money, Δ qty as a signed count** — the
  quantities are whole units (the engine's integers) while the prices are
  integer cents, so the deltas format the same way as their columns; breach
  lines render their deltas in red.
- **The invoice upload reuses the #33 flow verbatim**: `uploadAttachment`
  from `requisitions/new/upload.ts` with an invoice-scoped `UploadApi`
  (declare/complete against the invoice BFF routes, PUT straight to the
  presigned URL); `parseApiResult` is now exported from upload.ts instead
  of being duplicated. Upload error copy stays English-only there (the
  #33 precedent — the requisition wizard ships the same sentences).
- **`parseReason`/`parseOverBill` pin their regexes to the exact formats**
  (`inv_over_bill_qty: po_item=… invoiced=… received=… pct=…`,
  `inv_over_bill_price: po_item=… po_price=… invoice_price=… pct=…`,
  `inv_over_bill: po_number=… invoiced=… requested=… total=…`) and return
  null on any mismatch — the UI falls back to the raw string or the generic
  mapping rather than rendering a half-parsed sentence.

## Evidence

Against the local compose PostgreSQL
(`TEST_DATABASE_URL=postgres://eprocurement:eprocurement@localhost:55432/eprocurement?sslmode=disable`;
packages run sequentially `-p 1` per the dev-note 031 protocol):

- `make gen-proto` — only `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}` and the proto changed; the documents gen files
  showed line-ending-only churn and were reverted (`git diff --numstat`
  empty, then `git checkout --`).
- `go build ./... && go vet ./...` — clean (re-run after every step; the
  final full run with `TEST_DATABASE_URL` + `TEST_MONGO_URI` +
  `TEST_S3_ENDPOINT` set: clean). `gofmt -l` flags every file in the repo
  on this Windows checkout (CRLF — untouched files flagged identically),
  so the real formatting signal was vet/build plus review.
- `go test ./services/procurement/invoice/ -count=1` — green: the domain
  suite plus the new `TestValidateList` (14 table rows: defaults, all three
  statuses, status trims, `po draft` rejected as unknown, page/page_size
  clamps incl. the MaxPage bound and rejection, query trim/uppercase/
  wildcard escaping) and the store suites — `TestStoreListFilters`
  (descending creation order, header-only rows with **no match_result even
  on the fixture's matched/discrepancy rows**, the three status filters,
  lowercase-prefix over mixed-case numbers, literal-%, combined, tenant B
  sees only its own row, tenant A's prefix query never matches tenant B's
  number) and `TestStoreListPagination` (deep page, past-the-end total
  fallback, normalized defaults), plus the #39/#40 suites unchanged.
- `go test ./services/procurement/server/ -count=1` — green in the
  sequential sweep, including the new `TestInvoiceGRPCList` (defaults
  newest-first header-only via a viewer, the three status filters,
  lowercase prefix, combined, past-the-end fallback, cross-tenant listing,
  `inv_status_invalid`/`inv_page_invalid` 400s with exact messages,
  anonymous Unauthenticated) and `TestMapInvoiceError`'s two new arms.
- `go test ./services/gateway/server/ -count=1` — green, including the new
  `TestInvoiceListRouteThroughServerNew` (wired via `New` +
  `WithProcurement`; viewer passes `invoice:read`; envelope
  `{invoices,total,page,page_size}`; header-only row; trimmed filters
  forwarded; claims as gRPC metadata) and `TestInvoiceListRouteErrors`
  (401 anonymous before any procurement call; both list machine codes with
  their human sentences), plus the #39/#40 suites unchanged.
- Sequential sweep of the touched packages:
  `go test ./services/procurement/invoice/ ./services/procurement/server/
  ./services/gateway/server/ -count=1 -p 1` — all three `ok` in one run
  (the first sweep of the day hit the **known pre-existing flake** —
  `TestVendorGRPCList` failing `vendor_duplicate_code` on `L27-A`, the
  cleanup leak dev-notes 039/040 document and reproduced on pristine HEAD;
  wiped the leaked vendor rows per the documented protocol and re-ran the
  full sweep clean. The test tenants were wiped again after the run so the
  next full gate starts from a clean database).
- Migration 0012: `docker compose exec postgres psql … pg_indexes` lists
  `invoices_tenant_status_idx` on `procurement.invoices`;
  `procurement.schema_migrations` tops out at version 12.
- **EXPLAIN (ANALYZE, BUFFERS) — the status-filter shape** (decision 2's
  evidence), after seeding 60k invoices (10% matched) in one tenant under
  RLS bypass and `VACUUM (ANALYZE)`, run under the real RLS context
  (`SET LOCAL ROLE eproc_app` + `SET LOCAL app.tenant_id`):
  - Count arm (InitPlan): `Index Only Scan using
    invoices_tenant_status_idx`, `Index Cond: (status = 'matched')`,
    `Heap Fetches: 0`, `Buffers: shared hit=50`, ~2.4 ms — the exact shape
    0012 exists for (the 0011 parity claim).
  - Page arm: `Bitmap Index Scan on invoices_tenant_status_idx` (6,000
    rows) → Bitmap Heap Scan (1,112 heap blocks) → top-N heapsort on
    `(created_at DESC, id DESC)`; **Execution Time: 8.476 ms**.
  - Unfiltered count arm: `Index Only Scan` over the same index (60k
    entries, `Heap Fetches: 0`, ~22.4 ms) — the planner prefers 0012 even
    without the predicate because `tenant_id` leads it.
  - The scratch fixture was deleted and `VACUUM (ANALYZE)` re-run
    afterwards (table back to 0 rows).
- Web (vitest, `apps/web`): the 11 new files — `invoice-filters` (9),
  `invoice-eligibility` (21: hidden/shown tables for match/create +
  ready/pending attachment gates), `match-result` (8: parse/Δ/reason/fill),
  `InvoiceActions` (6: hard-gate disable + gate reason, POST + refresh,
  409 mapping, fallback, 401 dance), `MatchResultPanel` (5: pending,
  invalid, matched verdict with Δ 0, discrepancy verdict with +20 Δ and the
  localized qty reason, price breach with money-formatted parsed numbers),
  `InvoiceAttachments` (7: list, download mints + navigates, download
  failure, hidden upload for viewers, the declare→PUT→complete flow with
  the computed checksum, mime rejection), `InvoiceForm` (8 + parseOverBill:
  PO-price prefill, per-field validation without a network call, zero-price
  acceptance, POST body of filled lines + redirect, the parsed 409 amounts
  inline, duplicate mapping, 401 dance), and the four BFF route tests
  (401-without-call, local validation, forwarding + bearer, pinned 409
  passthrough, 401 passthrough, 502) — **all green (85 tests)**; the full
  suite `npx vitest run` — **342/342 green (48 files)**, including the
  pre-existing suites and the i18n parity test over the new dictionary
  sections.
- `npx tsc --noEmit` — 0 errors outside test files; the 53 errors inside
  pre-existing test files are unchanged from pristine `HEAD` (verified by
  stashing and counting on the clean tree).
- `npx next build` — green; new routes in the table: `/app/invoices`
  (171 B client — the server-component list), `/app/invoices/[id]`
  (3.16 kB), `/app/invoices/new` (2.5 kB), and
  `/api/invoices{,/[id]/match,/[id]/attachments,/[id]/attachments/
  [attachmentId]/complete,/[id]/attachments/[attachmentId]/download}`.
  `npx eslint .` — clean.
- Working-tree note: a mid-task `git stash` cycle left `HEAD` on `develop`
  (both branches sit on 4b650f7); `git checkout
  feature/41-invoice-ui-match-result` restored the branch with the whole
  working tree intact (`git status` verified).
- Not run by me (the workflow script re-runs the full gate set per the
  ask): the repo-wide `go test ./...` (I ran the three touched packages
  sequentially plus `go vet`/`go build` over everything) and the root-level
  `pnpm install --frozen-lockfile && pnpm build && pnpm lint && pnpm test`
  (I ran the app-level equivalents inside `apps/web`: `next build`,
  `eslint .`, `vitest run`). No compose or seed changes.

## Follow-ups

- **#56 (performance budgets)** owns the 100k invoice fixture + the p95
  budget table — this issue ships the index and the EXPLAIN shape only
  (decision 2).
- The upload panel's error copy reuses #33's English sentences
  (`uploadErrorMessages`); localizing that map would touch the requisition
  wizard too — fine to do together if finance asks.
- The Match button's gate recomputes from the embedded attachments on every
  `router.refresh()`; a long-running session never sees a stale gate after
  an upload because the upload itself refreshes the page.
- Pre-existing, unchanged (dev-notes 029–040): the procurement server test
  cleanup leak (L27/G-01 residue, hence `-p 1` and the before/after wipes),
  the `dbmigrate` advisory lock, `go test -race` unrunnable locally (no
  cgo).
