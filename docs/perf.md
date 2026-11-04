# Performance budgets, measurement, and index review (#56)

This document is the summary layer for everything performance in the
procurement domain: the PRD targets, the measurement protocol, the
per-endpoint budgets with their measured medians/p95s and plans, the
by-design sequential-scan exceptions, the N+1 audit, and how to re-run the
budget suite. The full measured-plan details (query-plan narratives per
feature) stay in the query-plan sections of
[docs/schema/procurement.md](schema/procurement.md); this page summarizes
and links.

## 1. PRD targets

| Source | Target | Enforcement |
|---|---|---|
| PRD §12 FR-09 (prd-v0.2.md) | dashboard p95 < 500 ms on seed data | `dashboard.TestStoreSummaryScale100k` |
| PRD §19 NFR, Performance row | p95 < 300 ms for list/detail endpoints on the 100k-item seed; dashboard p95 < 500 ms | the five scale tests (below) |
| PRD §23 DoD | "p95 latency meets NFR on seed dataset" | `make perf-budget` / CI `go test ./...` |

The FR-09 vs §19 dashboard inconsistency (300 vs 500 ms) was resolved by
measurement in #56 per the maintainer decision ("cek potensial optimasi,
jika memungkinkan optimasi menjadi < 300 ms, tetapi jika terdapat banyak
pertimbangan, update sesuaikan PRD menjadi < 500ms"): the clean dev fixture
reads p95 ≈ 199 ms — under 300 with the 0014 indexes already in the repo —
but the CI runner, where the budget is actually enforced, read median
~275–282 ms / p95 307–783 ms across three attempts (CI run 41290280970
round 1 failed a 300 ms trial budget on all three attempts). Closing that
gap would take a query redesign of the spend/pending widgets — "banyak
pertimbangan" — so FR-09 was reconciled to < 500 ms per §19 and the
enforced budget stays 500 ms. The lists' 300 ms row is enforced and passes
with ≥3× headroom (see §3).

## 2. What is measured — the fixture protocol

**Store level, not live HTTP.** Every budget below is measured by calling
the store's Go API against a **~100k-row `procurement.*` fixture the test
builds itself** (maintainer decision 2026-11-01 on #56). This is the
#27/#32/#38/#52 `scale_test.go` pattern — *not* HTTP calls against `make
seed` data: the seed fills the `demo.*` schema only, and synchronizing
`demo.*` into `procurement.*` is the separate demo epic (#9/#110) decision.
When that sync lands, an HTTP-level budget can reuse this doc's protocol and
budgets unchanged.

The shared protocol, identical in all five fixtures:

1. **Serialize the load phases** on the scalegate advisory lock
   (`internal/platform/scalegate`): the five 100k fixtures bulk-load
   hundreds of thousands of rows against one database, and concurrent loads
   starve each other past `go test`'s package timeout (CI run 41198373932).
   The gate releases before the measurement loops, which are short and
   retry-protected.
2. **Deterministic fixture, tracked expectations** — shape arithmetic in
   the row index (statuses, numbers, timestamps), every expected count and
   newest row tracked *while writing*, reconciled against the store before
   anything is timed (never re-derived from the system under test).
3. **Production steady state** — `VACUUM (ANALYZE)` (visibility map +
   planner statistics after a bulk load) and `CHECKPOINT` (so the background
   checkpointer does not compete with the timing loop).
4. **Warm-ups + timed runs** — 3 warm-ups, then 20 timed iterations per
   shape (30 in the pre-#56 fixtures), reporting median and p95.
5. **Retry protocol** — a shape whose p95 busts its budget retries after a
   5 s cooldown, up to 3 attempts, final attempt decisive: the budget is a
   steady-state claim, but `go test ./...` runs all five fixtures'
   measurements concurrently and a neighbour's bulk-load burst or CHECKPOINT
   can spike one loop (the #38 protocol, dev-notes 038/041/052).
6. **EXPLAIN (ANALYZE, BUFFERS)** of every shape's statement, logged on the
   passing attempt under the real RLS context (`SET ROLE eproc_app` +
   `app.tenant_id`) — the evidence mirrored into docs/schema/procurement.md
   and this page.

**Budgets: 300 ms p95 per list shape; 500 ms p95 per whole dashboard
summary (the reconciled FR-09/§19 number — §1).** Numbers in this page are
dev-machine measurements (Windows dev
host, `en_US.utf8`/libc collation — the dev collation inflates string work
several-fold vs the Linux/glibc CI database; see the requisition note in
docs/schema/procurement.md).

## 3. Per-endpoint budgets and measured shapes

All tables: ~100k rows in the tenant under test, the protocol above, budget
p95 < 300 ms per shape.

### Vendor list (#27; `vendor.TestStoreListScale100k`)

Recorded from the #27 budget test (full narrative in
docs/schema/procurement.md, "Vendor list"):

| Shape | median | p95 |
|---|---|---|
| unfiltered page 1 | ~73 ms | ~78 ms |
| status = deactivated (10% of rows) | ~15 ms | ~16 ms |
| code prefix (~1k rows) | ~57 ms | ~62 ms |
| name substring (~10k rows) | ~63 ms | ~66 ms |
| status + name substring combined | ~61 ms | ~74 ms |

### Requisition list (#32; `requisition.TestStoreListScale100k`)

Recorded from the #32 budget test ("Requisition list" in
docs/schema/procurement.md): unfiltered ~110 ms (count arm Index Only Scan
over `requisitions_tenant_idx`, page arm top-N over the `COLLATE "C"` sort);
status ~10 ms (`requisitions_tenant_status_idx`, 0003); number prefix and
combined are seq-scan shapes (~50–160 ms on the dev collation) — see §4.

### Purchase-order list (#38; `purchaseorder.TestStoreListScale100k`)

Recorded from the #38 budget test ("Purchase order list" in
docs/schema/procurement.md):

| Shape | median | p95 |
|---|---|---|
| unfiltered page 1 | ~100–101 ms | ~104 ms |
| status = issued (10% of rows) | ~14–16 ms | ~16–19 ms |
| number prefix (~1k rows) | ~42 ms | ~44 ms |
| status + number prefix combined | ~54–55 ms | ~57 ms |

### Invoice list (#56; `invoice.TestStoreListScale100k` — new in this issue)

The #41 list had the 0012 index but no scale fixture; #56 closes that.
Fixture: 16 vendors, 10k requisitions/POs, **100k invoices** (80/10/10
pending/matched/discrepancy, `INV-56xxxxx` numbers, `created_at` spread
monotonically over 30 days, one line each). Solo run 2026-11-01:

| Shape | median | p95 |
|---|---|---|
| unfiltered page 1 | ~79 ms | ~82 ms |
| status = matched (10% of rows) | ~11 ms | ~14 ms |
| number prefix (~1k rows) | ~49 ms | ~55 ms |
| status + number prefix combined | ~13 ms | ~18 ms |

`EXPLAIN (ANALYZE, BUFFERS)` per shape (100k rows, real RLS context, from
the passing run's log — abridged to the load-bearing nodes):

```text
-- unfiltered page 1: p95 ~82 ms
InitPlan 1 (returns $0)
  -> Aggregate
       -> Index Only Scan using invoices_tenant_idx   ← count arm
            Filter: (RLS tenant policy)   Heap Fetches: 0   Buffers: hit=82
            (100,000 rows, ~30 ms)
-> Limit → Sort (top-N heapsort, created_at DESC, id DESC)
     -> Seq Scan on invoices                          ← page arm, by design (§4)
          Filter: (RLS tenant policy)   Buffers: hit=1924   (~54 ms)

-- status filter ('matched', 10%): p95 ~14 ms
InitPlan 1 → Aggregate
  -> Index Only Scan using invoices_tenant_status_idx   ← 0012 index
       Index Cond: (status = 'matched')   Heap Fetches: 0   (~2.7 ms)
-> Limit → Sort (top-N)
     -> Bitmap Heap Scan on invoices
          -> Bitmap Index Scan on invoices_tenant_status_idx
               Index Cond: (status = 'matched')   (~4 ms of heap + sort on top)

-- number prefix 'INV-5609' (~1k rows): p95 ~55 ms
InitPlan 1 → Finalize Aggregate → Gather
  -> Parallel Seq Scan on invoices                    ← by design (§4)
       Filter: (RLS tenant policy AND upper(number) ~~ 'INV-5609%')
       Rows Removed by Filter: 49,502/worker   Buffers: hit=1924   (~23 ms)
-> Gather Merge → Sort (top-N) over the same parallel seq scan (~22 ms)

-- combined status+query ('discrepancy' + 'INV-5609'): p95 ~18 ms
InitPlan 1 → Aggregate
  -> Bitmap Heap Scan on invoices
       -> Bitmap Index Scan on invoices_tenant_status_idx
            Index Cond: (status = 'discrepancy')   (10k rows)
       Filter: (RLS tenant policy AND upper(number) ~~ 'INV-5609%')
       Rows Removed by Filter: 9,900   (~5.6 ms)
-> Limit → Sort (top-N) over the same bitmap scan
```

### Dashboard summary (#52, reconciled in #56; `dashboard.TestStoreSummaryScale100k`)

One `Summary` call = one transaction, five queries (pending count, pending
first page, spend totals, spend by vendor, PO status distribution). The 0014
partial indexes carry the pending and spend shapes. Budget: **p95 < 500 ms**
(§19; the #56 reconciliation of FR-09's < 300 ms figure is recorded in §1 —
the dev fixture measures comfortably under 300, the CI runner does not).
Clean solo dev run 2026-11-01: **median ~165 ms, p95 ~199 ms** (the
#52-era dev machine measured p95 265–456 ms against the same budget with a
churned database); CI round 1 (run 41290280970) read median ~275–282 ms /
p95 307–783 ms across attempts under the concurrent five-fixture run —
inside the 500 ms budget, outside a 300 ms one. Per-query plans and the
by-design distribution scan are in docs/schema/procurement.md, "Dashboard
summary".

### Suite-concurrency check (2026-11-01, all five fixtures in one run)

The dev-machine re-run of all five fixtures against one database with the
measurement loops overlapping (the CI-like condition the retry protocol
exists for). **Every shape passed on its first attempt — no retries fired:**

| Package | Shape | median | p95 |
|---|---|---|---|
| vendor | unfiltered page 1 | 86 ms | 91 ms |
| vendor | status filter | 16 ms | 18 ms |
| vendor | code prefix | 72 ms | 74 ms |
| vendor | name substring | 79 ms | 80 ms |
| vendor | combined status+query | 74 ms | 77 ms |
| requisition | unfiltered page 1 | 91 ms | 95 ms |
| requisition | status filter | 13 ms | 15 ms |
| requisition | number prefix | 38 ms | 43 ms |
| requisition | combined status+query | 49 ms | 60 ms |
| purchaseorder | unfiltered page 1 | 100 ms | 117 ms |
| purchaseorder | status filter | 15 ms | 16 ms |
| purchaseorder | number prefix | 42 ms | 45 ms |
| purchaseorder | combined status+query | 70 ms | 96 ms |
| invoice | unfiltered page 1 | 88 ms | 104 ms |
| invoice | status filter | 14 ms | 19 ms |
| invoice | number prefix | 52 ms | 53 ms |
| invoice | combined status+query | 14 ms | 15 ms |
| dashboard | summary (whole call) | 192 ms | 198 ms |

Every row is under its 300 ms list budget (dashboard's own budget is the
reconciled 500 ms) — with the worst shape (PO combined, p95 96 ms; lists
overall) at 3×+ headroom.

## 4. By-design sequential-scan exceptions

The AC "no sequential scans on hot paths" is **measured, not literal**
(maintainer decision 2026-11-01): no *avoidable* seq scans on selective
shapes, and the exceptions below are the planner's own measured cost
choices, documented per docs/schema/procurement.md:

1. **Unfiltered page 1 (every list).** The count arm already rides an index
   only scan (`*_tenant_idx`); the page arm seq-scans because it must visit
   every visible row anyway (`total` is exact) and the heap is cheaper than
   index+heap lookups at this scale — the #27 finding, unchanged at 100k.
2. **Whole-table aggregates on the dashboard** (PO status distribution, and
   the spend sums' window scans): every row is visited by design (zeros
   included / window arithmetic), so the planner's parallel seq scan is the
   correct plan; the 0014 partial predicates shrink the scanned set.
3. **The number/code-prefix shapes (the #56 finding).** The plan intended to
   close these with new 0015 indexes "wherever the planner demonstrably
   benefits". The measured answer is **nowhere**: candidate prefix indexes
   (`invoices (upper(number) text_pattern_ops)`,
   `requisitions`/`purchase_orders (number text_pattern_ops)`,
   `vendors (code text_pattern_ops)`), present and `ANALYZE`d, were **never
   chosen** by the planner at ~1k, ~100, or ~10 matching rows (1%, 0.1%,
   0.01% selectivity) under the real RLS context. The mechanism, verified by
   isolating the predicate under an identical plan context:

   - A user qual containing a **leaky function** — `~~` (LIKE) and `~~*`
     (ILIKE) are not leakproof (`textlike.proleakproof = f`) — can never
     become an index condition on a row-security-protected table. PostgreSQL
     pulls it out of the scan so the policy filter is guaranteed to evaluate
     first. With `enable_seqscan = off` the planner still seq-scanned, i.e.
     no index path existed at all.
   - The control: the same statement with **leakproof** range quals
     (`number >= 'X' AND number < 'Y'`) on a plain btree index produced
     `Index Scan … Index Cond` under the same RLS context — and the
     status-equality shapes keep their index/bitmap scans for the same
     reason (`texteq` is leakproof).

   Consequence: while the #23 single-policy RLS design stands, *no* prefix
   index can serve these lists. The experiment record is migration
   `0015_list_prefix_index_review.sql` (deliberately no DDL). The two paths
   that would change it are both out of scope here:
   - **RLS policy reshaping / per-service read roles** — the #27 trade-off
     (docs/schema/procurement.md, "Vendor list" note 2): the policy's
     bypass arm keeps tenant-leading index quals unavailable, and reshaping
     is a platform decision (golden rule 10), explicitly excluded by
     maintainer decision 2.
   - **A leakproof range-bound rewrite of the prefix predicates**
     (`number >= q AND number < successor(q)`): indexable under RLS per the
     control above, but computing a collation-safe successor for
     vendor-supplied free-form numbers (case/diacritic tailoring) is a
     query-semantics change with real correctness risk — follow-up, not a
     #56 change. The shapes measure 6× under budget as seq scans.

## 5. N+1 audit

**Write path — the four per-item INSERT loops, batched in #56.** Creation
of the four line-item aggregates issued one INSERT per item (one network
round trip each):

| Site | Table | Fix |
|---|---|---|
| `requisition/store.go` `insertItems` | `procurement.requisition_items` | `pgbatch.Exec` |
| `purchaseorder/store.go` `insertPO` | `procurement.po_items` | `pgbatch.Exec` |
| `invoice/store.go` `insertInvoice` | `procurement.invoice_lines` | `pgbatch.Exec` |
| `goodsreceipt/store.go` `insertGRN` | `procurement.goods_receipt_items` | `pgbatch.Exec` |

`internal/platform/pgbatch` sends the fixed statement once per row through a
single `pgx.Batch` — one pipeline round trip for the whole basket — while
every row keeps its own result, so the per-row error semantics are exactly
the plain loop's (first failing row's error surfaces; the transaction fails
as a whole either way). The invoice header's 23505 duplicate-number mapping
is untouched (it happens on the header's single-row INSERT before the
lines). `pgbatch`'s test covers the happy path (all rows land), the empty
no-op, and the failure path (a mid-batch 23505 surfaces and the transaction
rolls back); the four stores' suites pass unchanged.

**Read path — no N+1 by design.** The list endpoints are header-only: one
statement per list carrying the page and the exact total (the #32 shape —
`services/procurement/server/api.go` serves them without loading items or
steps; the detail reads own those). Aggregate detail loads fetch their lines
in one query per aggregate (`loadLines`/`loadItems`/`loadGRN`), and the
dashboard summary runs its five queries in one transaction. Nothing on the
read path issues per-row follow-up queries; the #56 reconciliation asserts
the invoice list rows stay header-only (`Items` empty, `match_result` empty).

## 6. Mongo index review (documents service)

The event-history and audit lists are covered by the pinned index sets in
`services/documents/history/store.go` (`EventIndexes`/`AuditIndexes`, PRD
§14.7): `uniq_event_id` (unique — also the consumer-dedupe mechanism),
`tenant_occurred` (`tenant_id, occurred_at DESC[, event_id DESC]` — the
ListEvents sort), and `tenant_entity_occurred` (the per-aggregate timeline
filter; events only). They are created idempotently on boot
(`EnsureIndexes`) and pre-created by the seeder before its bulk inserts
(mongod 7.0's WiredTiger invariant on million-row builds, dev-note 042).
#56's review found no list shape without a serving index and made no
change; the indexes are test-pinned so they cannot drift from the seeder.

## 7. How to re-run

```bash
make perf-budget        # scripts/dev/perf-budget.sh
```

Runs the five ~100k fixtures against the compose PostgreSQL (`make
compose-up` first; `TEST_DATABASE_URL` falls back to `.env`'s
`DATABASE_URL`) one package at a time with `-v`, then parses each shape's
final `median=… p95=… over N runs (budget Bms)` log line and prints a
PASS/FAIL row per shape (attributed per package) against the budget the
test itself declares. Exit code is non-zero if any shape's final p95
meets-or-exceeds its budget or any `go test` fails. The full run takes
~8 minutes; sequential packages measure each fixture against a quiet
database — the CI-like concurrency is exercised by `go test ./...`, whose
retry protocol absorbs contention spikes.

In CI the budgets run as part of `go test ./...` (the Go job) — there is no
separate perf-budget CI job yet (no CI change in #56; see follow-ups).

## 8. Follow-ups

- **RLS policy reshaping / per-service read roles** — would unlock
  tenant-leading index quals (and with a policy rewrite, leaky-qual
  indexing) for every list; platform decision per golden rule 10, excluded
  by maintainer decision 2 on #56.
- **Leakproof range-bound rewrite of the prefix predicates** — makes the
  prefix shapes indexable under the current RLS; needs a collation-safe
  successor computation for free-form numbers (see §4.3).
- **A dedicated perf-budget CI job with trend storage** — parse the same
  log lines into a time series instead of pass/fail gates only.
- **HTTP-level budgets** — once #9/#110 load `demo.*` data into
  `procurement.*` (or a dedicated HTTP fixture exists), repeat the protocol
  end-to-end through the gateway.
