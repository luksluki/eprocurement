# 056 — Performance budget, indexes, and EXPLAIN notes

- **Issue:** #56
- **Branch:** feature/56-perf-budget-indexes-explain-notes
- **Date:** 2026-11-01

> **Provenance:** the three open questions on issue #56 were answered by the
> maintainer's issue comment (recorded 2026-11-01, `gh issue view 56
> --comments`, run this session). Decisions applied:
>
> 1. **Measurement = fixture `procurement.*` at seed scale (100k) built by
>    the test script, measured at store level** (the #52/#32 scale_test
>    pattern) — not live HTTP against `make seed` data (that requires
>    #110's demo-loading).
> 2. **The seq-scan AC is measured, not literal**: no avoidable sequential
>    scans on selective shapes; new indexes close the prefix shape *wherever
>    the planner demonstrably benefits*; the by-design exceptions are
>    documented; **no RLS policy reshaping** (separate platform decision,
>    golden rule 10).
> 3. **Dashboard p95 target (verbatim):** "cek potensial optimasi, jika
>    memungkinkan optimasi menjadi < 300 ms, tetapi jika terdapat banyak
>    pertimbangan, update sesuaikan PRD menjadi < 500ms" — measured, then
>    branch A or B.

## What

- **`services/procurement/invoice/scale_test.go` (new)** — the invoice list
  gains the missing 100k-row store-level budget test (the #41 list had the
  0012 index but no fixture; its measured-plan section explicitly deferred
  this to #56). Tenant slot `eeeeeeee-0056-…`: 16 vendors, 10k
  requisitions, 10k POs (two `po_items` each), **100k invoices** (one line
  each; 80/10/10 pending/matched/discrepancy; `INV-56xxxxx` numbers;
  `created_at` spread monotonically over 30 days) bulk-loaded via batched
  multi-row INSERTs under the bypass context, on the #27/#32/#38/#52
  harness: scalegate gate around the load phase, deterministic fixture with
  expectations tracked at write time, `VACUUM (ANALYZE)` + `CHECKPOINT`, a
  reconciliation subtest (exact totals + newest page-1 row per shape +
  header-only rows), then 4 shapes × (3 warm-ups + 20 timed runs) with the
  3-attempt retry protocol, and `EXPLAIN (ANALYZE, BUFFERS)` of every shape
  logged on the passing attempt. Budget: p95 < 300 ms (the §19 list bar the
  vendor/requisition/PO lists are held to).
- **Migration `0015_list_prefix_index_review.sql` (new, deliberately no
  DDL)** — the durable record of the #56 index review: the prefix-index
  candidates were measured and the planner never used them (mechanism
  below), so the migration carries the experiment record instead of dead
  indexes. Version 0015 is consumed by the record; the next real index
  change takes 0016. Applies cleanly through `dbmigrate.Apply` (recorded in
  `procurement.schema_migrations`, verified below).
- **Write-path N+1 eliminated** — `internal/platform/pgbatch` (new): sends
  one fixed statement once per parameter row through a single `pgx.Batch`
  (one pipeline round trip per basket) while every row keeps its own
  result, so per-row error semantics are exactly the plain loop's. The four
  per-item INSERT loops now use it: `requisition/store.go` `insertItems`,
  `purchaseorder/store.go` `insertPO`, `invoice/store.go` `insertInvoice`
  (the header's 23505 duplicate-number mapping untouched — it fires on the
  header's single-row INSERT), `goodsreceipt/store.go` `insertGRN`.
  `pgbatch`'s own test covers the happy path, the empty no-op (nothing is
  sent), and the failure path (a mid-batch 23505 surfaces and the
  transaction rolls back).
- **Dashboard budget tightened 500 → 300 ms (maintainer decision 3, branch
  A)** — `dashboard/scale_test.go` `summaryBudgetMs` now enforces FR-09's
  300 ms, and PRD §19's dashboard cell was reconciled to < 300 ms (it read
  < 500 ms; the #52 test had kept the 500 ms pending this measurement).
- **`make perf-budget` + `scripts/dev/perf-budget.sh` (new)** — the p95
  budget re-run path: runs the five ~100k fixtures against the compose
  stack one package at a time with `-v`, parses each shape's final
  `median=… p95=… over N runs (budget Bms)` log line (the budgets live in
  the tests, not the script), and prints a PASS/FAIL row per shape
  attributed per package; non-zero exit if any final p95 meets-or-exceeds
  its budget or any `go test` fails. No CI change in this PR (golden rule
  10; follow-up).
- **`docs/perf.md` (new)** — PRD targets (FR-09/§19/DoD), the fixture
  measurement protocol, per-endpoint shape tables with medians/p95s and the
  invoice shapes' EXPLAIN output, the by-design seq-scan exceptions, the
  RLS trade-off pointer, the N+1 audit, the Mongo index review, and the
  re-run instructions. Measured-plan narratives stay in
  docs/schema/procurement.md (updated: the invoice list section replaced
  its #41 60k-row deferral with the #56 100k evidence and the leakproof
  finding; the vendor/requisition sections cross-reference it; the
  dashboard section records the tightened budget and clean measurement).

## Decisions

1. **Maintainer decisions 1–3 applied as recorded on the issue** (see
   Provenance). Decision 3 resolved as **branch A**: the clean-dashboard
   measurement (median ~165 ms, p95 ~199 ms with the 0014 indexes already
   in the repo) meets FR-09's 300 ms with ~34% headroom and required *no*
   new query/index work — no "banyak pertimbangan" — so the budget was
   tightened and §19 reconciled rather than the PRD relaxed to 500 ms.
2. **Migration 0015 ships as a no-DDL decision record, and that is the
   measured answer, not a shortcut.** The plan's "(final name/content
   decided by measurement)" clause resolved to zero indexes: candidate
   prefix indexes on all four list tables — invoices
   `(upper(number) text_pattern_ops)`, requisitions/purchase_orders
   `(number text_pattern_ops)`, vendors `(code text_pattern_ops)` — were
   created, `ANALYZE`d, and EXPLAINed at ~1k, ~100 and ~10 matching rows
   (1%, 0.1%, 0.01% selectivity) under the real RLS context
   (`SET ROLE eproc_app` + `app.tenant_id`): the planner kept the parallel
   seq scan in **every** case. The root cause is a row-security planner
   rule, verified by isolating the predicate shape:
   a user qual containing a **leaky function** (`~~` LIKE, `~~*` ILIKE —
   `textlike.proleakproof = f`) can never become an index condition on an
   RLS-protected table (the planner pulls it out of the scan so the policy
   filter evaluates first; with `enable_seqscan = off` the planner still
   seq-scanned, i.e. no index path existed at all), while the **leakproof**
   control (`number >= 'X' AND number < 'Y'` on a plain btree,
   `text_ge`/`text_lt.proleakproof = t`) produced
   `Index Scan … Index Cond` under the same context — the same reason the
   status equalities keep their 0003/0011/0012 bitmap scans. Closing the
   prefix shapes therefore requires the RLS policy reshaping / read-role
   split (platform decision, excluded by decision 2) or a leakproof
   range-bound rewrite of the predicates (a collation-sensitive
   query-semantics change — vendor-supplied free-form numbers make a safe
   "successor" computation non-trivial; recorded as a follow-up, and the
   shapes measure 6× under budget as seq scans).
3. **Experiment fixtures were ephemeral and cleaned up.** The SQL
   experiment harnesses (tenants `…0056/0057/0058/0059`, ~400k rows, four
   candidate indexes) were deleted, the indexes dropped, and the tables
   vacuumed after the measurements; only the checked-in test fixture
   (created/deleted by the scale test itself) and the 0015 record remain.
4. **The perf-budget script trusts the tests' own budgets** — the report
   parses `budget Bms` from each test's log line instead of duplicating the
   numbers in the script, so the script can never drift from the enforced
   budgets. The final logged attempt per shape is the decisive one (the
   tests' own retry semantics).
5. **Dev-note filename** follows the existing zero-padded convention
   (`055-prometheus-metrics-grafana-dashboard.md` → this file is
   `056-performance-budget-indexes-explain-notes.md`).

## Evidence

- `go build ./... && go vet ./internal/platform/pgbatch ./services/procurement/…`
  — clean.
- `go test ./internal/platform/pgbatch -count=1 -v` — 3/3 subtests PASS
  (happy path lands 3 rows; empty batch sends nothing; mid-batch 23505
  surfaces and the tx rolls back).
- `go test ./services/procurement/invoice -run TestStoreListScale100k
  -count=1 -v -timeout 20m` — PASS, first attempt on every shape (solo,
  2026-11-01): unfiltered median 79.1 ms / p95 81.6 ms · status 11.3/13.8 ·
  prefix 48.7/54.9 · combined 13.4/17.8 (budget 300 ms). Reconciliation
  subtest PASS (totals 100,000 / 10,000 / 1,000 / 100 and the tracked
  newest rows). The package's full suite (`go test
  ./services/procurement/invoice -count=1`) also PASSes (46 s).
- Store suites of the four batched packages, unchanged:
  `go test ./services/procurement/{requisition,purchaseorder,goodsreceipt,invoice}
  -count=1` — all PASS (their scale tests included).
- `go test ./services/procurement/dashboard -run TestStoreSummaryScale100k
  -count=1 -v -timeout 20m` on the cleaned database — PASS, first attempt:
  median 165.5 ms, p95 198.6 ms (against the old 500 ms budget; the
  tightened 300 ms budget then PASSed inside the full-suite run below).
  A first measurement taken while the experiment fixtures (~100k extra
  POs) were still loaded read p95 251 ms — discarded as polluted and
  re-measured after cleanup.
- Index experiments (psql against the compose PostgreSQL, all under
  `BEGIN; SELECT set_config('role','eproc_app',true);
  set_config('app.tenant_id',…);`):
  - Candidate prefix indexes present + `ANALYZE`d: seq scan at 1%, 0.1%,
    0.01% selectivity on all four tables (e.g. invoices prefix
    `INV-5609`: parallel seq scan, 1,924 buffers, ~22 ms, index untouched;
    same for requisitions `PR-5709`, POs `PO-5809`, vendors `VND-5909`).
  - `enable_seqscan = off` under RLS: still `Seq Scan` (cost 10000000000…
    penalty overridden) → no index path exists for the leaky LIKE.
  - Control: leakproof range quals + plain btree → `Index Scan … Index
    Cond` under RLS; leaky LIKE → seq scan; same statements without RLS →
    `Bitmap Index Scan` on the candidate index (path generation returns
    when the policy is out of the picture).
  - Cleanup verified: fixture tenants deleted, experiment indexes dropped,
    `VACUUM (ANALYZE)` + `CHECKPOINT` run, table counts back to baseline.
- `docker exec eprocurement-postgres-1 psql … -c "SELECT version, name FROM
  procurement.schema_migrations ORDER BY version DESC LIMIT 3"` —
  `15 | list_prefix_index_review | 2026-11-01` recorded (applied by the
  test harness's `dbmigrate.Apply` during the package runs).
- `make perf-budget` — two full validations on 2026-11-01:
  1. All five fixtures against one database with the measurement loops
     overlapping (the CI-like condition): **every shape passed on its first
     attempt — no retries fired**; worst rows: PO combined p95 96 ms,
     dashboard summary p95 198 ms vs the tightened 300 ms budget. The
     per-shape table is in docs/perf.md §3.
  2. The final script (sequential per package, per-package attribution —
     the first version collapsed identically-named shapes across packages,
     caught and fixed during this validation): exit 0, all 18 shapes PASS,
     dashboard summary p95 224 ms on a quiet database.
- `make help` lists `perf-budget`; `bash -n scripts/dev/perf-budget.sh`
  clean; the parser was additionally verified against a synthetic
  busting-attempt log (FAIL row + exit 1) and both real logs.
- **Full gate** — `go build ./...`, `go vet ./...`, and `go test ./...`
  (with `TEST_DATABASE_URL`/`TEST_MONGO_URI`/`TEST_S3_ENDPOINT` from `.env`):
  build/vet clean; `go test ./...` exited 1 on five packages, every failure
  traced to a cause outside this diff:
  - `documents/objectstore` + `worker/export` — "Endpoint url cannot have
    fully qualified paths" from minio-go: **pre-existing on this machine**,
    proven by stashing the whole diff and running the same two packages at
    HEAD (identical failure). The repo diff touches nothing in documents/
    worker.
  - `documents/history TestApplyIdempotent` and
    `procurement/server TestVendorGRPCList` — pass solo with the diff
    (`ok` re-runs recorded above/below); the full-suite failures were
    parallel-run flakes over the shared Mongo/Postgres state. The server
    one surfaced a pre-existing trap in that test (its `t.Cleanup` deleting
    the `L27-%` seeds is registered only after the seeds succeed, so one
    interrupted run wedges every later run on `vendor_duplicate_code`); the
    four leftover rows were deleted by hand to restore the baseline and the
    package re-ran green. No server/vendor code path is touched by this
    diff.
  - Every diff-affected package passed inside the same full run:
    `internal/platform/pgbatch`, the four batched stores, the invoice and
    dashboard scale tests, and `procurement/vendor` (170 s incl. its scale
    fixture).
- `docker compose config --quiet` — clean; `docker compose ps` — all
  services healthy.

## Follow-ups

- **RLS policy reshaping / per-service read roles** — the only path that
  makes tenant-leading (and, with a policy change, leaky-qual) index use
  possible for every list; platform decision (golden rule 10), explicitly
  excluded here by maintainer decision 2.
- **Leakproof range-bound rewrite of the prefix predicates** — indexable
  under the current RLS (the control experiment proves it), but needs a
  collation-safe successor computation for free-form vendor numbers;
  collation pitfalls make it a design conversation, not a drive-by.
- **A dedicated perf-budget CI job with trend storage** — parse the same
  log lines into a time series instead of pass/fail only (no CI change in
  #56 per golden rule 10).
- **HTTP-level budgets** — once #9/#110 load `demo.*` data into
  `procurement.*`, repeat the protocol end-to-end through the gateway.
- **`procurement/server` test hygiene (pre-existing, out of scope here)** —
  `TestVendorGRPCList` registers its `t.Cleanup` only after its seeds
  succeed, so an interrupted run wedges every later run on
  `vendor_duplicate_code`; moving the cleanup registration above the seeds
  (and a leftover-row pre-delete) would close the trap.
- **MinIO test endpoint quirk (pre-existing, out of scope here)** —
  minio-go rejects this machine's `TEST_S3_ENDPOINT=http://localhost:9000`
  ("Endpoint url cannot have fully qualified paths"), failing
  `documents/objectstore` and `worker/export` at HEAD as well; CI uses
  values that parse. Worth normalizing in a follow-up (host:port form or
  explicit scheme handling in `objectstore.New`).
