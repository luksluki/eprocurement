# Dev note — #39 Invoice model and create API

- **Issue:** #39
- **Branch:** feature/39-invoice-model-create-api
- **Date:** 2026-10-27

> **Provenance:** the maintainer answered all three open questions on
> 2026-10-27 (issue #39 comments), so no agent-taken decision needed the
> 035/036/037 protocol this time:
>
> 1. **Attachments:** demo-flow enforcement — #39 ships full attachment
>    support (entity type + routes + `GetInvoice` with attachments
>    embedded); the "required" obligation is the demo flow's (UI #41
>    requires it and/or the #40 match guard rejects without a ready
>    attachment). The create API contract stays unchanged: attachments are
>    declared after entity creation per the architecture, with no
>    cross-service atomicity.
> 2. **Over-billing guard:** cumulative — the sum of a PO's existing
>    invoices plus the new one must be ≤ `po.total_cents` (the #37
>    `qty_received` precedent; prevents staged over-billing).
> 3. **Invoicable PO statuses:** the GRN set — `acknowledged |
>    partially_received | received`; draft/issued/cancelled/closed answer
>    409 `po_illegal_transition`.

## What changed

- **Migration `0009_invoices.sql`** — `procurement.invoices` (FK `po_id`
  and FK `vendor_id`, both without cascade — invoices are financial
  records; `UNIQUE (tenant_id, vendor_id, number)` — the issue's technical
  note: vendor invoice numbers are unique **per vendor**, so the same
  number under a different vendor is a different invoice; `currency`
  CHECK IDR/USD; `status` CHECK `pending|matched|discrepancy` DEFAULT
  `'pending'` — #40's values ship in the CHECK from day one, so #40 never
  needs an ALTER; `match_result jsonb` NULL, reserved for #40) and
  `procurement.invoice_lines` (`invoice_id` FK CASCADE, `po_item_id` FK,
  CHECK `qty > 0`, CHECK `unit_price_cents >= 0`, UNIQUE
  `(invoice_id, po_item_id)`). RLS tenant-scoped on both (lines derive
  tenancy from the parent invoice via `EXISTS`, the 0008 pattern); grants
  SELECT/INSERT/UPDATE for `eproc_app`. Loaded automatically by the
  `//go:embed *.sql` in migrations.go.
- **`services/procurement/invoice` (new)** — the domain (`invoice.go`:
  request-shape validation before any transaction opens — number non-empty
  after trimming and ≤ 100 chars, non-empty lines, qty > 0,
  unit_price ≥ 0, unique line ids; `TotalCents` recomputes the total
  server-side from the lines; `ErrOverBill` sentinel + typed
  `*OverBillError` whose `Error()` is the pinned detail line, the #37
  `OverReceiptError` twin) and the store (`store.go`): `Create` runs the
  pinned step order in one tenant transaction — validate shape → lock the
  PO (`purchaseorder.LoadForUpdate`, the shared serialization point) with
  **vendor_id and currency read from the locked aggregate** (never client
  input) → line membership (`inv_po_item_unknown`) → server-side total →
  **cumulative strict over-billing guard** (SUM of the PO's invoices +
  this one ≤ `po.total_cents`; violation → the typed error) → status gate
  (the GRN set; reused `purchaseorder.ErrIllegalTransition`) → insert
  header + lines (23505 on `invoices_tenant_vendor_number_uidx` →
  `inv_number_duplicate`, the vendor `mapWriteError` pattern) → backstop
  re-SUM post-insert inside the transaction (unreachable under the lock,
  answers the same typed error if it ever fires). `Get` returns header +
  lines, tenant-scoped, cross-tenant indistinguishable from missing.
  **No outbox row** (the §14.6 catalog names `invoice.matched` /
  `invoice.discrepancy` only — #40's; the vendor/requisition creation
  precedent) and **no PO mutation** (no `qty_invoiced` column, no status
  move — the store test asserts `purchase_orders` status/updated_at/
  updated_by stay byte-identical across a create).
- **proto + gen + wiring** — additive `CreateInvoice` / `GetInvoice` RPCs
  with `Invoice` / `InvoiceItem` / request messages (`match_result_json`
  empty while pending; #40 owns the content). `make gen-proto` regenerated
  only the procurement gen files. procurement `server.New` takes the new
  store (main.go constructs it); `methodPermissions` maps `CreateInvoice` →
  `rbac.InvoiceWrite` (finance/admin only) and `GetInvoice` →
  `rbac.InvoiceRead`; `api.go` adds the handlers, `invoicePB`, and
  `mapInvoiceError` (the typed over-bill error carries its pinned message
  verbatim; shape codes → 400, guards → Aborted/409, duplicate →
  AlreadyExists/409, not-found → 404).
- **documents** — `attachment.EntityTypeInvoice` joins `entityTypes`
  (path-safety whitelist) and `entityPermissions` gains
  `{rbac.InvoiceRead, rbac.InvoiceWrite}`. The tests that used `"invoice"`
  as an invalid-entity string — `attachment_test.go:25`,
  `api_test.go` (create-validation and list-unknown-entity), and a fourth
  spot the plan's list missed, `attachment/store_test.go:100` — now use
  `"contract"`; a positive `entity invoice` row and a new
  `TestAttachmentInvoiceEntity` (finance declares/completes, buyer denied
  `requires invoice:write`, viewer lists/downloads, cross-tenant list
  empty) cover the new entity.
- **gateway** — `ProcurementClient` widened with `CreateInvoice` /
  `GetInvoice` (the compile-forced stubs joined `fakeRequisitionClient`,
  which `fakePOClient` embeds); new `invoices.go`: `POST /api/v1/invoices`
  behind `RequirePermission(rbac.InvoiceWrite)` (body `{po_id, number,
  items: [{po_item_id, qty, unit_price_cents}]}`, trimmed at the edge,
  201 + the stored invoice), `GET /api/v1/invoices/:id` behind
  `invoice:read` embedding `attachments` when documents is wired (the
  vendors.go parity; absent otherwise), the dedicated over-bill renderer
  (409, body `code` exactly `inv_over_bill`, body `error` the full pinned
  detail line, every other code through the shared `grpcHTTPError`), and
  the messages map. `attachments.go` gains `entityInvoice` +
  `registerInvoiceAttachmentRoutes` (declare checks the invoice exists
  first — the vendor pattern; complete/download bind the attachment to the
  path id) and `createInvoiceAttachment`. `server.go` registers the routes
  under `WithProcurement`.
- **Docs** — `docs/schema/procurement.md` (new "Invoices (#39)" section:
  tables, store contract, the pinned over-bill format, statuses, the
  attachment decision, API + error codes; follow-ups list gains #39 and
  the #40/#41 pointer), `docs/schema/documents.md` (invoice entity +
  routes + permission columns), `docs/rbac.md` (#39 landed entry).

## Decisions

- **Guard order: line membership → cumulative over-billing → status gate,
  all before any mutation** (the `ApplyReceipt` order). The
  cumulative-before-edge order is pinned, the #37 precedent: a billing
  that cannot fit answers `inv_over_bill` even on a PO whose status would
  also reject (verified: an over-budget invoice on a closed PO answers
  the typed error, while a fitting one answers `po_illegal_transition`) —
  the amount problem is the more precise diagnosis and the one a finance
  user can act on.
- **Strict, tolerance-free at create** — `invoiced + requested ≤ total`
  with no epsilon. FR-08's configurable tolerance (qty ±%, price ±%) is
  the #40 match's concern; blending it into create would let over-bills
  through before the match ever runs.
- **`po_number` (not po id) in the pinned detail line** — the plan pinned
  `inv_over_bill: po_number=<n> invoiced=<i> requested=<r> total=<t>`;
  a finance user resolving a vendor dispute speaks PO numbers, and the
  id stays discoverable through the request's po_id.
- **Number handling: trim at both edges, bound 100, free-form** — no
  charset rule (vendor document numbers are their issuer's convention;
  the DB stores text, the UNIQUE index compares exact trimmed values).
  Uniqueness is per `(tenant, vendor)` only, per the issue's technical
  note.
- **`vendor_id`/`currency` derived inside the transaction from the locked
  PO** — not merely ignored-if-sent: the create request carries no such
  fields at all (proto + REST body have neither), so a client cannot
  even attempt to spoof them; the invoice's vendor is by construction
  the PO's vendor at lock time.
- **`inv_po_item_unknown` (invoice-prefixed), not the reused grn code** —
  the aggregate-relative guards of #37 live on the purchase-order
  aggregate (`grn_po_item_unknown`); invoice line membership is checked
  by the invoice store against the locked aggregate, so it gets its own
  `inv_` code. The status guard, by contrast, is literally the PO state
  machine's rule, so `po_illegal_transition` is reused (the plan's call).
- **`GetInvoice` returns `match_result_json` as a string** — empty while
  pending; #40 owns the content and can keep it a verbatim JSON pass-
  through (the `payload_json` precedent in documents' ListEvents).
- **grpcCleanupRequisition (procurement server tests) now deletes
  invoices before purchase_orders** — invoices hold no-cascade FKs to
  POs, so the shared cleanup would otherwise fail underneath any
  leftover invoice rows; the invoice suite's own tenants (88888888-…)
  and vendor prefix (`INV-39-` / `G-INV39-`) stay out of the older
  suites' way.

## Evidence

Against the local compose PostgreSQL (`TEST_DATABASE_URL` derived from
`.env`; documents tests against compose MongoDB/MinIO with
`TEST_MONGO_URI` + `TEST_S3_ENDPOINT=localhost:9000` — the CI format,
docs/schema/documents.md L113; packages run sequentially per the
dev-note 031 dbmigrate protocol):

- `make gen-proto` — only `gen/go/procurement/v1/{procurement.pb.go,
  procurement_grpc.pb.go}` and the proto changed (`git status --short`).
- `go build ./... && go vet ./...` — clean (re-run after every step below).
- `go test ./services/procurement/invoice/ -count=1` — green: the domain
  suite (`TestValidateNumber`, `TestValidateItems`, `TestTotalCents`,
  `TestOverBillError` — the pinned line verbatim + the sentinel match)
  and the store suites — `TestStoreCreateInvoiceHappyPath` (trimmed
  number, vendor/currency derived from the locked PO, server-recomputed
  total, status pending, stored `match_result` NULL; outbox count equal
  to the harness baseline (submit/decide/convert/issue/acknowledge rows)
  — the invoice added none; `purchase_orders` status/updated_at/updated_by
  byte-identical before/after; Get with lines; invoicing still legal on
  partially_received and received; the exact fit
  15,000,000 + 450,000 == 15,450,000 passes; post-budget create answers
  the pinned line `inv_over_bill: po_number=PO-… invoiced=15450000
  requested=150000 total=15450000`; only the two `grn.posted` rows joined
  the outbox), `TestStoreCreateInvoiceOverBill` (cumulative rejection
  with the exact pinned message; concurrency — two parallel 8,000,000
  creates on a 15,450,000 PO: exactly 1 win + 1 typed loss, 1 invoice
  row), `TestStoreCreateInvoiceGuards` (empty/over-bound number, empty
  items, zero qty, negative price, duplicate line, unknown line, bad po
  id, unknown po — machine codes verbatim; draft PO →
  `po_illegal_transition`; closed PO: over-budget → the typed over-bill
  answer, fitting → `po_illegal_transition` (the pinned order);
  cross-tenant → `po_not_found`), `TestStoreCreateInvoiceDuplicateAndRead`
  (same vendor + same number → `inv_number_duplicate`; same number under
  a different vendor → accepted; Get bad id → `inv_id_invalid`, missing →
  `inv_not_found`, cross-tenant → `inv_not_found` indistinguishable).
- `go test ./services/procurement/server/ -count=1 -run 'TestInvoiceGRPC|TestMapInvoiceError'`
  — green (full-flow gRPC: requester/buyer/approver/viewer →
  PermissionDenied on create; the machine-code table; over-bill → Aborted
  with the exact pinned line; draft → `po_illegal_transition`; happy path
  with derived vendor/currency, recomputed total 15,150,000, status
  pending, empty `match_result_json`; zero `invoice.*` outbox rows;
  duplicate → AlreadyExists; GetInvoice detail/bad-id/cross-tenant;
  anonymous → Unauthenticated; the mapping table incl. typed + bare
  over-bill arms).
- `go test ./services/gateway/server/ -count=1 -run TestInvoice` — green:
  `TestInvoiceRouteThroughServerNew` (route wired via `New` +
  `WithProcurement` + `WithDocuments`; finance 201; trimmed po_id/number/
  item ids forwarded; claims as gRPC metadata; GET detail embeds
  attachments via the documents fake with entity `invoice`),
  `TestInvoiceRouteDetailWithoutDocuments` (attachments key absent),
  `TestInvoiceRouteRequiresPermission`
  (requester/approver/buyer/viewer → 403 with `required: invoice:write`
  before any procurement call; admin passes),
  `TestInvoiceRouteErrors` (over-bill → 409 with `code` exactly
  `inv_over_bill` AND `error` carrying the full detail line; the route's
  other codes keep the shared mapping with human sentences; malformed
  JSON → 400 `body_invalid` without a procurement call),
  `TestInvoiceAttachmentRoutes` (declare checks the invoice first —
  unknown → 404 before any documents call; documents receives entity
  `invoice`; buyer denied declare; viewer may download).
- `go test ./services/documents/attachment/ ./services/documents/server/ -count=1`
  — green with `TEST_MONGO_URI` + `TEST_S3_ENDPOINT` set: the renamed
  invalid-entity cases (`contract` — attachment_test.go, api_test.go, and
  the store_test.go:100 twin), the new `entity invoice` row, and
  `TestAttachmentInvoiceEntity` (finance declare/complete, buyer denied
  `requires invoice:write`, viewer list/download, cross-tenant list
  empty) plus the pre-existing suites.
- `go vet ./... && go build ./... && go test ./...` (with
  `TEST_DATABASE_URL` + `TEST_MONGO_URI` + `TEST_S3_ENDPOINT` set) —
  clean: 29 packages ok, zero failures.
- Affected packages sequential sweep (the dev-note 031 protocol):
  `go test ./services/procurement/invoice/ ./services/procurement/purchaseorder/ ./services/procurement/goodsreceipt/ ./services/procurement/server/ ./services/gateway/server/ -count=1 -p 1`
  — all green in one run from a cleaned database.
- `docker compose exec postgres psql … \d procurement.invoices` (and the
  lines twin) — 0009 applied with the documented columns,
  `UNIQUE (tenant_id, vendor_id, number)`, `UNIQUE (invoice_id,
  po_item_id)`, the two CHECKs + the status CHECK, both FKs (CASCADE on
  `invoice_id` only), forced RLS with the transitive lines policy, and
  grant rows for `eproc_app`; `procurement.schema_migrations` lists
  `invoices`.
- `gofmt -w` applied to every touched Go file; `go vet ./...` re-run clean.
- pnpm build/lint/test not run by me (no TypeScript touched — the same
  justification as dev-note 037); the workflow script re-runs the full
  gate set after this dev note, per the ask.
- No compose or seed changes (the seed's demo invoices live in their own
  `demo` schema, scripts/seed/schema.sql L86-94, untouched).

## Follow-ups

- **#40 (3-way match):** writes `match_result`, moves status
  pending → matched/discrepancy, emits `invoice.matched` /
  `invoice.discrepancy` (§14.6), and enforces FR-08's configurable
  tolerance — create deliberately has none.
- **#41 (invoice list + UI):** the list endpoint
  (`GET /api/v1/invoices`), the `/app/invoices` pages, and the demo-flow
  enforcement of "attachment required" (this API never demands one).
- Idempotency-Key on `POST /api/v1/invoices` — the same conscious gap as
  #37's posting route (a client retry that still fits records twice);
  the decide-key table pattern is the candidate fix if finance asks.
- Pre-existing, unchanged (dev-notes 029/030/031/035/036/037): the
  procurement server test cleanup leak (L27/G-01 residue, hence `-p 1`
  and the before/after wipes), the `dbmigrate` advisory lock,
  `go test -race` unrunnable locally (no cgo).
