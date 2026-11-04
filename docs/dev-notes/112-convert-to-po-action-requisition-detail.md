# Dev note — #112 Convert-to-PO action on the requisition detail page

- **Issue:** #112
- **Branch:** feature/112-convert-to-po-action-requisition-detail
- **Date:** 2026-11-05

## What changed

- **`lib/po-eligibility.ts`** — new pure helper **`canCreatePOFromRequisition(roles,
  requisitionStatus)`**: true only for a `po:write` holder (the existing
  `holdsPOWrite` — buyer/admin) on an `approved` requisition. The #34/#38
  "hidden when not eligible" pattern: the helper only decides rendering; the
  gateway re-checks `po:write` and procurement re-checks the status edge
  (`409 po_requisition_not_approved`). Table-driven cases added to
  `po-eligibility.test.ts` (roles × statuses, incl. multi-role).
- **`lib/gateway.ts`** — three additive changes: `listVendors` accepts an
  optional `status` filter (the gateway already supports `status=active`,
  `services/gateway/server/vendors.go:243`; values `active`/`deactivated`,
  `services/procurement/vendor/vendor.go:107`); new
  **`createPurchaseOrderFromRequisition(accessToken, requisitionId, vendorId,
  forwardedFor)`** posting `{vendor_id}` to
  `POST /api/v1/purchase-orders/from-requisition/:id` (the #35 endpoint that
  already exists — no gateway change); and the `GatewayResult` **ok-branch is
  widened with an optional `status?: number`** (set from the HTTP response in
  `callGateway`) so the 201-created vs 200-replay distinction can travel.
- **`lib/api-client.ts` + `lib/bff.ts`** — the same optional `status?: number`
  on `ApiResult`'s ok-branch (set in `callApi`), and `forwardGatewayResult`
  now prefers `result.status ?? successStatus`. Backward-compatible: every
  existing caller's `successStatus` matches the gateway's actual status
  (verified: requisitions/invoices/attachments/goods-receipts 201, exports
  202, the rest 200 — see Evidence), so behavior is unchanged for them; the
  body still rides through verbatim.
- **BFF route `POST /api/purchase-orders/from-requisition/[id]`** (new, the
  #38 issue-route pattern): reads the httpOnly access cookie (401
  `unauthorized` without a gateway call), mirror-validates the body locally —
  malformed JSON → 400 `body_invalid`, missing/blank/non-UUID `vendor_id` →
  400 `po_vendor_id_invalid`, both without calling the gateway (the
  goods-receipts mirror pattern) — forwards the bearer + the caller's
  `X-Forwarded-For` (#44 audit `actor_ip` for the `po.created` row), and
  forwards the gateway's 201/200 distinction. Gateway 4xx bodies ride through
  verbatim; 401 stays the refresh-dance signal; other failures → 502
  `gateway_unavailable`.
- **`ConvertToPO.tsx`** (new client component, the ApprovalActions/POActions
  pattern) + **`page.tsx` wiring**: the page computes eligibility
  server-side and, only when convertible, loads the active-vendor options
  (`listVendors(…, { status: "active" })`, pageSize 100 = the gateway's
  `MaxPageSize`) and passes them down. The requisition carries no vendor (PRD
  §13 + the #35 decision), so the picker is part of the action: submit stays
  disabled until a vendor is chosen; a failed vendor load (null) and an empty
  active list each render their own actionable message instead of a dead
  form. Success follows the #35 contract — **201 = draft PO created, 200 =
  idempotent replay "already converted" (not an error)** — both render a
  confirmation with the PO number and a link to
  `/app/purchase-orders/{id}`, then `router.refresh()` re-renders the
  server-rendered header (the requisition is now `closed`, so the action
  disappears). Errors map through `copy.errorMessages` (409
  `po_requisition_not_approved`, 400 `po_vendor_id_invalid`/`body_invalid`,
  404 `vendor_not_found`/`requisition_not_found`, 412 `vendor_deactivated`,
  403 `permission_denied` for a lost-race role loss, 502
  `gateway_unavailable`); 401 runs the refresh dance.
- **i18n** — new `requisitionDetail.convert` block on the `Dictionary` type
  and in both dictionaries (ID + EN); the shared type makes ID/EN parity a
  build-time guarantee.

## Decisions

- **"PO-create permission" = `po:write` (buyer/admin), not the finance
  role.** The issue text says "finance-authorized", but its acceptance
  criterion says "users with the PO-create permission" and the repo pins that
  permission to buyer/admin (`docs/rbac.md:22,38` — `po:write` row and the
  landed #35 note; the gateway 403s finance on the endpoint). The button
  gates through `holdsPOWrite`; finance never sees it, and a finance token
  that reaches the route anyway gets the gateway's 403 mapped.
- **Replay 200 is a success surface, not an error** (the #35 contract): the
  BFF preserves the gateway's 200, and the component shows the
  "already converted" copy with the same PO link. In practice the replay
  path is a race (another buyer converted between render and submit) because
  a converted requisition is `closed` and the button no longer renders.
- **Optional `status?: number` on the ok-branches instead of a new result
  type.** Smallest coherent widening: `callGateway`/`callApi` always set it,
  every existing consumer ignores it, and `forwardGatewayResult` falls back
  to its `successStatus` argument when absent. An alternative (a dedicated
  discriminated result just for conversion) would touch every shared-lib
  caller for one endpoint's need.
- **Vendor options load server-side, only when the action renders.** The
  detail page is a server component; the picker needs no client-side
  fetching, and non-buyers pay no extra round trip. `pageSize: 100` covers
  the tenant's vendors up to the gateway's own cap; a tenant with more than
  100 active vendors is a pagination follow-up, not a #112 surface.
- **`permission_denied` is mapped in the error copy** although the button is
  role-gated client-side: a token whose roles changed server-side since
  render should still get a human sentence, mirroring how the lost-race
  conversion gates read.

## Evidence

All commands run from `apps/web` (no Go file changed; the Go gates are
re-run by the workflow script after this note, per the ask):

- `pnpm vitest run "src/lib/po-eligibility.test.ts" "src/app/api/purchase-orders"
  "src/app/(app)/app/requisitions/[id]/ConvertToPO.test.tsx"` — **48 passed
  across 6 files**, including the new ones:
  `from-requisition/[id]/route.test.ts` (12: 401 without cookie and no
  gateway call; malformed JSON → 400 `body_invalid` locally; empty/blank/
  non-UUID `vendor_id` → 400 `po_vendor_id_invalid` locally; 201 forwarded
  with path id + `vendor_id` body + bearer + `X-Forwarded-For`; gateway 200
  replay stays 200; 409/412/404/403 machine codes ride through; gateway 401
  rides through; 5xx and network errors → 502 `gateway_unavailable`) and
  `ConvertToPO.test.tsx` (9: vendor options render + submit disabled until
  picked; null → load-failed message; `[]` → no-active-vendors message; POST
  path/body; 201 vs 200 pick the created/replayed copy with the PO number +
  `/app/purchase-orders/po-1` link + `router.refresh()`; 409 → mapped
  message, no refresh; unmapped code → generic; 401 → refresh dance with the
  session-expired notice).
- `pnpm vitest run` (the full suite — the shared-lib widening touches every
  consumer) — **390 passed across 53 files**, confirming no regression in
  the existing 201/202 forwarding routes (goods-receipts, invoices,
  exports).
- `pnpm build` — `✓ Compiled successfully`, route list includes
  `ƒ /api/purchase-orders/from-requisition/[id]`, exit 0.
- `pnpm lint` — clean, exit 0. `node scripts/assert-ui-css.mjs` — ok.
- `npx tsc --noEmit` reports errors only in pre-existing test files
  (Timeline/approval-eligibility/match-result/issue-route tests); verified
  pre-existing by stashing this diff and counting the same class of errors
  on the clean tree (47 `error TS` lines baseline; `next build` — the actual
  gate — passes regardless, and passed above with this diff applied).
- Forwarding-status compatibility audit for the widened ok-branch (grep of
  `forwardGatewayResult` callers vs the gateway handlers' success statuses):
  requisitions POST 201↔201, attachments declare 201↔201 (all three),
  goods-receipts 201↔201, invoices 201↔201, exports 202↔202, and the
  default-200 callers (decide/submit/match/complete/issue/acknowledge/
  runs/download-url/PUT requisition) all sit behind gateway handlers that
  answer plain `c.JSON` (200).
- No compose or seed changes.

## Follow-ups

- A tenant with more than 100 active vendors sees a capped picker (the
  gateway's `MaxPageSize`); a searchable vendor picker (type-ahead over
  `q`) is the natural upgrade if demo data grows past the cap.
- The requisition timeline still does not show the conversion (the #35
  `pr.converted`/`pr.closed` outbox-event gap, dev-note 035) — unchanged
  here; `router.refresh()` makes the header status the visible signal.
- Pre-existing, unchanged: the `tsc --noEmit` noise in test files (see
  Evidence) and the CRLF gofmt working-copy listing (dev-notes 029–049).
