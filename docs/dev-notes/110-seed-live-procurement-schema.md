# Dev note — #110 Seed the live procurement/identity schemas (demo epic #9)

- **Issue:** #110 (demo epic #9, `area:seed`)
- **Branch:** feature/110-seed-live-procurement-schema
- **Date:** 2026-11-06

> **Provenance:** maintainer decision in the #110 comments — attachments are
> OUT of scope (option a: nothing is written to MongoDB `document_meta` nor
> MinIO). The review constraints on the plan were folded in: the writer also
> covers `identity.user_roles` (roles live there, not on `identity.users` —
> without the rows every login would carry an empty role list and fail
> `rbac.Check` on all three AC-1 endpoints); `procurement.approval_rules` IS
> seeded (a two-step IDR chain, so AC 2(b)'s "mirror of the tenant's
> approval_rules" has an object and demo tenants can run live submits without
> hitting 412 `approval_rules_missing`); the determinism proof below states
> its same-anchor-hour condition; and the loadtest-tenant citation is
> docs/dev-notes/61-load-test-script-results.md:30-31 (not dev-note 63, which
> documents the onboarding/e2e tenants).

## What changed

- **`scripts/seed/main.go`** — the generator is now a pure function
  (`buildDataset(cfg, rng, base) → dataset`) with separate writers, and it
  loads the LIVE schemas in addition to the `demo.*` reconciliation export:
  - `identity.users` (+ `identity.user_roles` referencing the fixed
    `identity.roles` catalog): one user per role per tenant, all with the
    documented fixture password `demo-password`. The argon2id PHC string is
    built in the seed (salt from the seed RNG — `auth.phcEncode` is
    unexported and `auth.HashPassword` uses `crypto/rand`), same parameters
    as `services/identity/auth/password.go`, verified by
    `auth.VerifyPassword` in the unit test.
  - `procurement.vendors`, `procurement.approval_rules` (two-step IDR chain:
    step 1 `approver` threshold 0, step 2 `finance` threshold 10,000,000
    cents), `requisitions` + `requisition_items` + `approval_steps`,
    `purchase_orders` + `po_items`, `goods_receipts` + `goods_receipt_items`,
    `invoices` + `invoice_lines`.
  - Approval steps come from the real `approval.Materialize` over the seeded
    rules; decided steps carry `decided_by` = the seeded holder of the
    step's `role_required` (approver/finance users, disjoint from the
    requester pool {admin, requester, buyer}), so `decided_by ≠ requester_id`
    holds by construction; rejected requisitions have step 1 rejected with a
    reason and later steps skipped with no decision recorded (the
    `approval.go` Decide semantics).
  - Invoice verdicts come from the real `match.Run` engine at the strict
    0%/0% defaults (no `match_settings` row is seeded — absence IS the
    default), billing exactly the received qty at the PO price, so the
    stored `match_result` JSON is byte-identical to a live match run.
  - EVERY writer-owned column is pinned by the generator —
    `created_at`/`updated_at`/`decided_at`/`granted_at` included; no reliance
    on the tables' `DEFAULT now()`.
  - The per-tenant number counters (`requisition_counters`, `po_counters`,
    `grn_counters`) are deliberately NOT written: seeded numbers are a random
    per-tenant subset of the global `PR/PO/GRN-%06d` streams, and the stores'
    lazy init from `max(number)` (`requisition/store.go:94`, 
    `purchaseorder/store.go:480`, `goodsreceipt/store.go:214`) continues the
    sequence — writing counter rows would misstate the counts.
  - PO vendors are drawn from the PO's OWN tenant (the old generator drew
    from all tenants; the dashboard's spend widget joins `po → vendors` under
    tenant RLS, so a cross-tenant vendor would silently drop the PO from
    spend).
- **Reset (`resetData`)** — authoritative, per-tenant, both stores:
  - The tenant list is read from `SELECT id FROM demo.tenants` BEFORE the
    truncate — never replayed from the current flags — so `make seed-reset`
    with default flags removes the tenants of earlier non-default runs
    (`--tenants 5`, another `--seed`, …).
  - PostgreSQL: per-tenant `DELETE … WHERE tenant_id = ANY($1::uuid[])` in
    FK order (invoices → goods_receipts → purchase_orders →
    decision_idempotency → requisitions → the three counters →
    approval_rules → match_settings → outbox → vendors →
    `identity.users`), the `demo-story.sh:127-147` order; item/line/step
    children ride their parents' `ON DELETE CASCADE`, `user_roles`/
    `refresh_tokens` cascade from `identity.users`.
  - MongoDB: `deleteMany({tenant_id: {$in: seededTenants}})` on `events` and
    `audit_log` — replacing the old `Drop` (main.go:530-551 pre-change),
    which destroyed the history of every other tenant sharing the
    collections. Indexes are kept (no drop/rebuild cycle — the dev-note 042
    WiredTiger concern no longer applies; `CreateOne` before insert remains
    as the first-run/no-op path).
- **`scripts/seed/main_test.go`** — generator-level table-driven tests:
  determinism (same cfg+seed+anchor ⇒ `reflect.DeepEqual` datasets, plus the
  PR-000001 spot-check), full invariant sweep (totals, steps/actors,
  qty_received, matched-invoice placement, per-tenant number uniqueness,
  role catalog per tenant), PHC hash determinism + `auth.VerifyPassword`
  accept/reject, and the seedBase hour-truncation guard.
- **`scripts/seed/live_schema_test.go` (new)** — integration tests against a
  real PostgreSQL (skip without `TEST_DATABASE_URL`, the
  `dashboard/store_test.go:33-36` pattern): applies the identity +
  procurement migrations, writes a small generated dataset through the real
  writer, and asserts the invariants as SQL (including that NO counter or
  `match_settings` rows appear), plus the cross-tenant denial case: under
  `SET LOCAL ROLE eproc_app` + `app.tenant_id = A`, every tenant-carrying
  `procurement.*` table and `identity.users` expose exactly tenant A's rows.
- **`README.md`** — demo login table (`admin@<slug>.example` … password
  `demo-password`), seed/reset semantics for the live schemas, and the #110
  paragraph replaced.

## Decisions

1. **`identity.user_roles` is part of the writer** (review constraint):
   `identity.users` has no role column; login resolves roles via
   `LEFT JOIN user_roles/roles` (`auth/service.go:299-306`) into the JWT, and
   `rbac.Check` fails closed on an empty role list — without `user_roles`
   rows AC 1 would 403 on every endpoint. Reset is safe: the rows cascade
   from `identity.users`.
2. **`approval_rules` ARE seeded** (review constraint, recommendation taken):
   the two-step chain makes AC 2(b)'s "mirror of the tenant's rules" real,
   fills `approval_steps.threshold_cents` (NOT NULL) from a defined source,
   and lets a demo tenant run a live submit without 412
   `approval_rules_missing` (the demo-story.sh:40-41 lesson).
3. **Counters are not seeded** (issue AC): the numbers a tenant receives are
   a random subset of the global streams, so any counter row would be wrong;
   `max(number)` lazy init owns continuation. Reset still deletes counter
   rows of seeded tenants so a rerun after a live session does not leave a
   jumped sequence.
4. **Determinism protocol carries a same-anchor-hour condition** (review
   constraint): every timestamp hangs off `seedBase()` = now−120d truncated
   to the hour, so two runs inside one anchor hour produce identical values;
   runs in different hours differ in timestamp columns only. The proof below
   ran both passes inside one hour (03:47/03:48 UTC).
5. **`match_settings` and attachments are NOT seeded** (issue AC /
   maintainer): absence = the effective 0%/0% strict defaults, consistent
   with seeded invoices whose qty/price are identical to the PO; nothing is
   written to `document_meta` or MinIO.
6. **Deliberate reset limitation** (review constraint, documented): the Mongo
   reset touches only `events` and `audit_log` (the two collections the seed
   writes). `document_meta`, `notifications`, and `export_runs` rows of a
   seeded tenant created by LIVE sessions (outbox → worker) survive the
   reset as orphans once their Postgres parents are gone — the seed does not
   presume to reset collections it never writes; `make reset` remains the
   nuclear option. (`demo-story.sh` resets five collections because its
   story DOES create those rows live.)
7. **Citation correction carried into this note**: the live loadtest tenant
   `77777777-…` evidence is `docs/dev-notes/61-load-test-script-results.md:30-31`;
   dev-note 63:136-139 documents the onboarding `1111…` and e2e `3333…`
   tenants. Both facts motivated the deleteMany fix; only the citation was
   wrong in the issue text.
8. **Seed data coexists with non-demo tenants**: onboarding/e2e/demo-story/
   loadtest tenants already live in `procurement.*`/`identity.*`; the seed
   writes only its own tenants and the reset deletes only what
   `demo.tenants` lists. `make seed` therefore requires a migrated stack
   (compose services apply migrations at startup).

## Evidence

All commands run 2026-11-06 against the running compose stack
(gateway healthy, PostgreSQL on `:55432` per `.env`).

- **AC 1 — gateway reads per tenant after `make seed`** (login
  `POST /api/v1/auth/login`, password `demo-password`):

  | tenant (slug) | login | GET /api/v1/requisitions | GET /api/v1/purchase-orders | dashboard/summary |
  |---|---|---|---|---|
  | nusantara-retail | 200 | total 33,374 | total 22,240 | pending 7,358; spend IDR 30d 845,877,386,834 / 90d 2,525,611,603,553; distribution sums to 22,240 |
  | kirana-logistics | 200 | total 33,132 | total 22,105 | spend IDR 90d 2,502,389,526,785 |
  | sagara-manufacturing | 200 | total 33,494 | total 22,249 | spend IDR 90d 2,517,663,282,377 |

  `GET /api/v1/me` for the nusantara admin returns
  `{"roles":["admin"],"tenant_id":"b14b843e-…","user_id":"0479af10-…"}` —
  the `user_roles` rows resolve into the JWT.
- **AC 2 — invariants over the full 100k dataset** (SQL, all counts 0):
  requisition/PO/invoice `total_cents ≠ Σ(qty × unit_price)` → 0 rows;
  approval steps vs status (approved/closed → all approved; rejected →
  step 1 rejected + later skipped; submitted → all pending) → 0 violations;
  decided steps without a role-holding non-requester `decided_by` → 0;
  `po_items.qty_received ≠ Σ goods_receipt_items` → 0; `matched` invoices on
  a PO without GRN or outside acknowledged/partially_received/received → 0.
  Sample row (PR-000001, sagara): step 1 `approver` thr 0 approved by the
  approver user, step 2 `finance` thr 10,000,000 approved by the finance
  user, `updated_by` = last decider ≠ requester.
- **AC 3 — cross-tenant denial**:
  `TEST_DATABASE_URL=… go test ./scripts/seed -run TestLive` → PASS
  (2 tests). `TestLiveRowsTenantIsolation` asserts, under
  `SET LOCAL ROLE eproc_app` + `app.tenant_id=A`, the scoped count of every
  tenant-carrying table (vendors exactly `cfg.vendorsPer`, rules 2,
  requisitions/POs/GRNs/invoices = dataset counts, outbox/counters/
  decision_idempotency/match_settings 0, `identity.users` 5) and that
  tenant B's child rows (requisition_items, approval_steps) are invisible.
- **AC 4 — reset isolation, Postgres + Mongo**:
  - `make seed-reset` after the full seed: demo tenants → 0 requisitions,
    0 users, `demo.*` empty; other tenants' requisitions 4,387 and per-user
    counts (1111…:6, 2222…:3, 3333…:3, 4444…:1, 5555…:2, 6666…:2, 7777…:1)
    identical before/after.
  - Mongo before reset: `events` = {3333…:32, demo tenants ≈333k each};
    after: `events` = {3333…:32}, `audit_log` = {3333…:20} — identical
    before/after for the non-demo tenant. (The onboarding 1111… events
    documented in dev-note 63 were already gone — destroyed by the OLD
    seed's collection Drop on an earlier reseed; exactly the failure mode
    this change removes.)
  - Authoritative tenant list: `--tenants 2 --requisitions 50 --seed 7`
    then default-flag `--reset` → `demo.tenants` 2→0, those tenants' live
    rows → 0, other tenants still 4,387.
- **AC 5 — no counter rows**: integration test asserts 0 rows in
  `requisition_counters`/`po_counters`/`grn_counters` for the seeded
  tenants right after the writer; reset's DELETE list still includes them.
- **AC 6 — determinism, two full runs, seed=42, same anchor hour** (starts
  03:47:16 and 03:48 UTC): identical headline counts
  (requisitions 100,000; POs 66,594; GRNs 39,950; invoices 13,452; events
  1,000,000) and byte-identical spot-check dumps — every column of each
  tenant's earliest requisition, all approval steps of PR-000001…000006,
  and INV-000001…000003 including the `match_result` JSON (`diff` clean).
  `TestBuildDatasetDeterministic` pins the same property at the generator
  level; `TestPHCHashVerifies` pins the hash (verify-true for
  `demo-password`, verify-false for a wrong one, stable string per seed).
- **AC 7 — budget**: `make seed` → "seed: completed in 66s (budget 120s) OK"
  (first full run; subsequent runs 50–55s). The delta vs the old ~17s
  demo-only seed is the per-tenant DELETE reset (~10s) plus ~700k additional
  COPY rows into the live tables — measured inside the gated command.
- **AC 8 — reconciliation `demo.*` vs `procurement.*` scoped to the demo
  tenants** (diff = 0 on every shared table): requisitions 100,000/100,000;
  requisition_items 200,000/200,000; approval_steps 199,709/199,709;
  purchase_orders 66,594/66,594; po_items 133,188/133,188; goods_receipts
  39,950/39,950; invoices 13,452/13,452; vendors 300/300; users 15/15.
- **Per-tenant live row counts** (tenant order by creation):

  | # | reqs | req_items | steps | POs | po_items | GRNs | invoices | vendors | users |
  |---|---|---|---|---|---|---|---|---|---|
  | 1 | 33,132 | 66,264 | 66,158 | 22,105 | 44,210 | 13,289 | 4,331 | 100 | 5 |
  | 2 | 33,494 | 66,988 | 66,907 | 22,249 | 44,498 | 13,282 | 4,532 | 100 | 5 |
  | 3 | 33,374 | 66,748 | 66,644 | 22,240 | 44,480 | 13,379 | 4,589 | 100 | 5 |

- **Gates**: `go vet ./... && go build ./...` clean; `go test ./...` (unit,
  integration skipped) exit 0; `go test ./scripts/seed` PASS;
  `TEST_DATABASE_URL=… go test ./scripts/seed -run TestLive` PASS. pnpm
  build/lint/test not affected (no web changes) — run by the workflow's
  gate step.

## Follow-ups / risks

- The Mongo reset leaves live-session `document_meta`/`notifications`/
  `export_runs` orphans of seeded tenants (decision 6); if that ever
  confuses a demo, extend the reset to the five `demo-story.sh` collections.
- `make seed` now requires the migrated compose stack (it writes
  `procurement.*`/`identity.*`); against a bare database the role lookup
  fails with a pointed error instead of a confusing COPY failure.
- The 100k-scale seed spends ~2.3s computing 15 argon2id hashes; if the
  tenant/user count ever grows by orders of magnitude, share one hash per
  tenant or lower the per-user cost — not worth it at demo scale.
- The seeded events in Mongo are synthetic (`pr.*` types over seeded
  requisitions) and do not replay the procurement outbox; a live session's
  outbox-driven worker will project further `events`/`audit_log` rows for
  the demo tenants, which the next reset removes per-tenant.
