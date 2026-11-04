# Dev note — #63 Demo script and seeded story

- **Issue:** #63 (epic #12, milestone M5, `area:demo`)
- **Branch:** feature/63-demo-script-seeded-story
- **Date:** 2026-11-02

> **Provenance:** the two parked questions were answered on the issue
> (maintainer override welcome in PR review): (1) self-install through the
> gateway — the #59 / onboarding §5.1 pattern with a dedicated demo tenant
> UUID, NOT waiting for #110; (2) `make demo` / `make demo-reset` are in
> scope, Makefile change flagged `touchesInfra` (the `make e2e` precedent).

## What changed

- **`scripts/dev/demo-story.sh` (new)** — the installer behind
  `make demo`: an explicit per-tenant reset followed by an idempotent
  install.
  - *Reset (tenant `55555555-…` only):* one PostgreSQL transaction deleting
    the demo tenant's rows from every tenant-scoped `procurement.*` table —
    `invoices`, `goods_receipts`, `purchase_orders`,
    `decision_idempotency`, `requisitions`, the three persistent counters
    (`requisition_counters`, `po_counters`, `grn_counters` — without them
    the lazy init at max(existing number) keeps allocating 000002…),
    `approval_rules`, `match_settings`, `outbox`, `vendors` — plus the two
    demo users from `identity.users` by email (cascades `user_roles`,
    `refresh_tokens`). Item/line/step children ride their parents'
    `ON DELETE CASCADE`. Then MongoDB `deleteMany({tenant_id})` on **five**
    collections: `events`, `audit_log`, `document_meta`,
    `notifications`, and `export_runs`.
  - *Install (idempotent):* two users via `cmd/useradmin` (ON CONFLICT
    upsert), one vendor through the gateway (409-tolerant), the
    single-step IDR approval chain (`PUT /api/v1/approval-rules`), and
    non-default match tolerances qty 5% / price 2%
    (`PUT /api/v1/match-settings`).
- **`docs/demo-script.md` (new)** — the 5-minute presenter runbook mapping
  PRD §22 steps 1–7 to the real UI, with expected output recorded from the
  clean runs below (numbers, badges, verdict, panel rows, CSV content),
  the pre-demo observability-profile step, a determinism contract (what is
  identical vs what differs: UUIDs, timestamps, the
  `purchase-orders-manual-{HHMMSS}.csv` clock segment), the MinIO-orphan
  limitation of the reset, and honest UI-gap notes.
- **`Makefile`** — `demo` and `demo-reset` targets (+ help entries,
  `.PHONY`), the only infrastructure change in this PR (`touchesInfra`,
  maintainer decision #2). `demo` passes `DATABASE_URL` from `.env` the
  same way `make e2e` does.
- **`README.md`** — one line linking the demo runbook from the demo
  section added by #62.

## Decisions

- **One demo tenant (`55555555-…`), not two.** The plan draft carried
  5555/6666; PRD §22's story is a single tenant with four role
  narratives, and a second tenant doubles the install and the login
  pressure (identity's 5/min-per-email limiter is the onboarding §5.1
  trap) while adding nothing to the 5-minute click path. Tenant switching
  already has its own demo (onboarding §5.1, tenants 1111/2222).
- **Two users, both admins: requester + approver.** Self-approval is
  forbidden for everyone, admins included (`docs/rbac.md`,
  `apps/web/src/lib/approval-eligibility.ts`), so a single user cannot
  run the story; the second user decides. Both hold the admin role (the
  #59 fixture pattern), so PRD §22's buyer/finance legs run under the
  same two logins — two logins per demo, well inside the rate budget.
- **`export_runs` IS reset (the plan review's catch).** The Reports page
  the demo's step 5 shows renders exactly this collection, and manual
  runs upsert on `{tenant_id, scheduled_for}` = the event's
  `occurred_at` — unique per run — so without the delete the second demo
  run would show two history rows and break the twice-identical AC.
  Cleaning `notifications` (not UI-observed) while leaving the one
  observed collection dirty would have been incoherent.
- **Approval rules and match settings are installed, not just users.**
  Submitting a PR without rules fails `412 approval_rules_missing`, and
  the demo's tolerance step needs a stored row to show; both are
  tenant-scoped tables the reset wipes, so both belong to the idempotent
  install list. Tolerances are non-default (5%/2%) so the step displays a
  configured value (0%/0% is the effective default when no row exists).
- **Vendor is pre-created; the doc says so.** `/app/vendors` is a
  read-only list (no create form) — the runbook's step 1 shows the list
  and notes the gap instead of pretending a create flow.
- **Attachment steps are explicit in the click path.** The PR attachment
  is draft-window-only (declare is rejected once submitted), and the
  invoice **Run match** button stays disabled until an attachment row is
  `ready` — both verified live and written into the runbook.
- **Over-receipt presentation.** The UI path rejects inline
  ("Quantity exceeds this line's remaining 1." plus the form-level
  "Fill at least one line…"); the server backstop is `409
  grn_over_receipt` with the pinned detail line (verified via curl) —
  the doc names both and does not claim the UI shows the 409.
- **Reset does not touch MinIO bytes** — documented as a limitation
  (orphan objects accumulate; `make reset` is the nuclear option), not
  silently ignored.

## Evidence

Full stack healthy (`docker compose ps`: all services up, observability
profile included). Web app served with `pnpm --filter
@eprocurement/web start` (the #59 production-server pattern). The story
was driven through the real UI (in-app browser; file uploads executed
through the app's own declare→presigned-PUT→complete pipeline from the
page context, since the embedded browser cannot drive a file chooser)
and through the gateway where the UI has no surface (convert, #112).

- `bash -n scripts/dev/demo-story.sh` — clean.
- **Run 1** (`bash scripts/dev/demo-story.sh`): installer summary
  printed; then the UI story: EN locale via the header switcher;
  wizard Details→Items→Attachments→Review (total "Total (cents):
  15,000,000 IDR"); **"Requisition submitted: PR-000001"**; requester's
  own detail has no approval buttons; approver login → **Approve** →
  badge `approved`, timeline gains `pr.approved` + `pr.step_approved`
  within ~4 s; convert via the doc's curl block → **PO-000001** (draft);
  UI **Issue** → `issued` → **Acknowledge vendor** → `acknowledged`;
  partial receipt qty 1 → **"Receipt recorded: GRN-000001"**, badge
  **`partially_received`**, items 2/1/1; over-receipt attempt (2 vs
  remaining 1) → inline "Quantity exceeds this line's remaining 1." +
  "Fill at least one line with a quantity greater than 0."; server
  backstop via curl → `HTTP 409 {"code":"grn_over_receipt", …}`;
  invoice **INV-DEMO-001** (qty 1 at PO price) → `pending`, Run match
  disabled until the PNG attachment row reads `ready` → **verdict
  "Matched"**, badge `matched`, panel row 2/1/1, Δ qty 0, Δ price
  IDR 0, flag `ok`; dashboard (server HTML verified — spend 30/90
  IDR 150,000, PT Demo Makmur row, partially_received: 1); Reports →
  **Run export now** → one row `success`+`manual`, Rows 1; MinIO object
  `exports/55555555-…/2026-11-02/purchase-orders-manual-055314.csv`
  containing exactly the `PO-000001,…,partially_received` row;
  `GET /api/v1/match-settings` → qty_pct 5, price_pct 2.
- **Resilience (step 6):** `docker compose stop worker` → POST
  `/api/v1/exports` still `202 {"event_id":…,"status":"requested"}`
  (run history unchanged — nothing processed, nothing lost);
  `docker compose start worker` → the queued run appears exactly once
  (second `manual` row, fresh timestamp), no duplicates.
- **Tracing (step 7):** Jaeger `:16686` API lists services
  gateway/procurement/identity/documents/worker; gateway operations
  include `/procurement.v1.Procurement/IssuePurchaseOrder`; worker spans
  name the outbox events (`outbox publish po.acknowledged`).
- **Run 2** (`make demo`): installer output byte-identical to run 1
  (`diff` of the captured logs: only make's echoed recipe line differs).
  Post-reset state verified in the stores — demo tenant: 0 requisitions,
  0 counters, 0 outbox, 1 rule, 1 match row, 1 vendor; Mongo: 0 rows in
  all five collections; onboarding tenant 1111… kept its 24 requisitions
  and events, e2e tenant 3333… kept its 7. Story re-driven: UI submit →
  **PR-000001** again (with the PR attachment `ready` while still a
  draft); approve; convert → **PO-000001**; issue/acknowledge; partial
  GRN → **GRN-000001**, `partially_received`; over-receipt → 409
  `grn_over_receipt`; **INV-DEMO-001 accepted again** (the reset freed
  the (tenant, vendor, number) uniqueness — the key repeat-proof); match
  → `matched:true` with the same 2/1/1 line; export trigger → exactly
  one `manual` success row.
- `make -n demo demo-reset` — expands to the intended recipe lines (note:
  `make -n` still evaluates the `$(shell sed … .env)` assignments at
  parse time — a read-only effect).
- `docker compose config -q` — clean (no compose change in this PR).
- `go vet ./... && go build ./...` — clean; `go test ./...` — 34 packages
  ok (no Go code touched).
- `pnpm install --frozen-lockfile && pnpm build && pnpm lint && pnpm
  test` — green (3/3 build, 4/4 lint, 6/6 test; no workspace dependency
  changed).

## Follow-ups

- **#112** — the convert-to-PO UI; when it lands, demo step 3 loses its
  terminal detour and the runbook's curl block goes away.
- **#110** — when the seed loads the live schema, revisit whether
  `make demo` should derive from `make seed` data instead of
  self-installing.
- MinIO orphan objects from repeated demo resets: a console-side bulk
  delete or a reaper extension is a possible small follow-up; `make
  reset` covers it today.
- The tolerance config step has no UI (admin/settings is a placeholder) —
  a settings page would replace the curl in demo step 4.
