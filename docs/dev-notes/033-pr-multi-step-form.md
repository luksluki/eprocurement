# 033 — PR multi-step form with validation and attachments

- **Issue:** #33
- **Branch:** feature/33-pr-multi-step-form
- **Date:** 2026-10-25

## What

- **`GetRequisition` end-to-end (the draft-restore enabler).** New RPC in
  `proto/procurement/v1` (+ `make gen-proto`), handler in
  `services/procurement/server/api.go`, and an entry in `methodPermissions`
  (`requisition:read`). It is the detail read the #32 list deferred: header
  **plus items and approval steps** (the bodies list rows never carry).
  Gateway: `GET /api/v1/requisitions/:id` which, when documents is wired,
  **embeds `attachments: [...]`** (vendor-detail parity) so the wizard's
  reload restores header + items + attachment list in one round trip.
- **Requisition attachments (documents + gateway).** `entity requisition`
  joined `vendor` as an accepted owner kind (`attachment.ValidEntity`);
  object keys follow `attachments/{tenant}/requisition/{uuid}`. The
  attachment gRPC methods left the interceptor's `methodPermissions` map on
  purpose — their permission depends on the entity type the interceptor
  cannot see — and each handler now re-checks a per-entity permission table
  (`vendor:read/write` vs `requisition:read/write`) via `rbac.Check`. New
  gateway routes `POST /api/v1/requisitions/:id/attachments` (declare;
  existence via `GetRequisition` **plus the requester-ownership rule
  mirrored at the edge**, admins excepted), `.../complete`, and
  `.../download` (entity-id binding, vendor parity).
- **The wizard (`/app/requisitions/new`, PRD §11 four steps).**
  RHF + Zod mirror the domain rules exactly (currency ∈ {IDR, USD} with no
  default, per-item description non-blank / qty > 0 / unit_price_cents ≥ 0,
  ≥ 1 item to submit); "Next" triggers the step's schema slice and the
  final submit re-validates. Autosave creates the server draft as soon as
  the currency is chosen (the domain rejects empty currency, so creation
  waits), PUTs the basket on valid changes (800 ms debounce), and the URL
  carries `?draft=<id>`. Reload restores everything through
  `GET /api/requisitions/:id`; a non-draft restore flips the form read-only
  with autosave stopped. Attachments upload from the browser:
  sha256 → declare (BFF) → **presigned PUT straight to MinIO** → complete,
  with machine codes mapped to per-step messages.
- **BFF routes with verbatim 4xx passthrough (a deliberate deviation from
  the #32 thin-proxy pattern).** New routes `POST /api/requisitions`,
  `GET|PUT /api/requisitions/[id]`, `[id]/submit`, `[id]/attachments`,
  `[id]/attachments/[attachmentId]/complete|download` forward gateway 4xx
  bodies (`{error, code}`) unchanged; only 401 stays the refresh-dance
  signal and 5xx/network collapse to 502. The #32 list route keeps its
  canonicalizing behavior — it never sees 4xx in practice — but the wizard
  routes must carry `requisition_forbidden` / `requisition_illegal_transition`
  / `attachment_too_large` / … to the UI (AC3/AC4).
- **Web plumbing:** `lib/gateway.ts` gained typed calls whose results carry
  parsed error bodies (`GatewayResult`), `lib/bff.ts` (cookie + result
  forwarding), `lib/api-client.ts` (client-side BFF caller), i18n
  `requisitionForm` section (ID + EN), and a "New requisition" link on the
  requisitions list page.

## Decisions

- **New web dependencies (golden rule 10 disclosure):**
  `react-hook-form@7.91.0`, `zod@4.6.7`, `@hookform/resolvers@6.0.0` — the
  substance is mandated by issue #33 ("RHF + Zod validation"; PRD §20 stack
  row "React/Next + TanStack Query/Table + Zustand + RHF/Zod"), recorded
  here per the dev-note 026 precedent ("New dependency: minio-go v7.0.83").
  zod v4 works with `zodResolver` v6; numbers register via RHF
  `valueAsNumber` (no schema coercion) so resolver input/output types match.
- **Route sanction.** `GET /api/v1/requisitions/{id}` and the requisition
  attachment routes are **not** in PRD §15; they are sanctioned by the #33
  issue decision (agent comment, 2026-10-25). docs/schema notes say so.
- **Declare ownership at the edge (review finding, fixed).** `GET /:id` is
  `requisition:read` (tenant-wide) so the existence check alone would let
  requester B attach to requester A's draft. The declare handler therefore
  mirrors procurement's `requireOwner` (requester or admin → else 403
  `requisition_forbidden`). Complete/download keep vendor-parity binding
  only (`entity_id` must match the path) — same residual exposure the
  vendor routes accept, listed under Follow-ups.
- **Currency locked after draft creation.** `UpdateRequisitionRequest`
  carries `{id, items}` only, so changing currency would require recreating
  the draft — which would orphan attachments bound to the old `entity_id`.
  The select disables once `draftId` exists, with an explanatory note.
- **Autosave keying (bug found by tests, worth remembering).** The autosave
  effect keys on the basket's **serialized string**, not array identity,
  and holds its callbacks in a ref: callers rebuild the items array (and
  wrappers like `useRouter`) every render, and an identity-keyed effect
  cancels the in-flight request on every unrelated state change — the
  wizard deadlock the first test run exposed.
- **BFF error contract.** 4xx verbatim + 401 passthrough + 502 otherwise
  (see What). The wizard maps codes per step: field errors from Zod, upload
  errors from `uploadErrorMessages` (incl. the neutralized
  `attachment_entity_invalid` → "Attachment owner type is not supported."),
  submit errors from a small table (`approval_rules_missing` included).
- **Non-draft restore is read-only.** A submitted-from-another-tab
  requisition would 409 every autosave PUT; the wizard detects
  `status != draft` on restore, shows a notice, and disables navigation and
  submit (test-pinned).
- **Read-only wizard note.** IAB cannot drive a native file chooser, so the
  live proof fed the input a `File` via `DataTransfer` and dispatched
  `change` — the component itself then ran declare → PUT → complete in the
  real browser (real CORS preflight against MinIO).

## Evidence

- **Go:** `go vet ./...` and `go build ./...` clean;
  `TEST_DATABASE_URL=…eproc_test_33… TEST_MONGO_URI=… TEST_S3_ENDPOINT=localhost:9000
  go test ./... -count=1` — **all packages ok** (procurement server,
  requisition, documents server + attachment, gateway server included).
- **New tests per layer (happy + failure):**
  - procurement `TestRequisitionGRPCGet`: detail with items+steps,
    same-tenant other-requester read, cross-tenant NotFound, bad-id 400,
    anonymous Unauthenticated, and a no-roles call pinned to the
    interceptor message (proving the `methodPermissions` entry re-checks).
  - documents `TestAttachmentRequisitionEntity`: requester
    declares/completes/lists/downloads a requisition attachment;
    requester→vendor declare 403; buyer→requisition complete 403 (both
    messages pin the required permission); unknown entity 400 on the read
    path; tenant B list empty. `TestCreateUploadValidation` and the
    attachment/store tests moved their invalid-entity cases to `"invoice"`
    and add requisition/vendor list isolation.
  - gateway: detail route embeds attachments (+ nil-documents degradation),
    error mapping; declare owner-check (foreign draft 403, admin 201,
    unknown 404 before documents), complete/download binding + 412 mapping,
    viewer 403s, and the neutralized entity message.
  - web: **134 vitest tests / 21 files green** (`pnpm vitest run`) —
    schema table-driven, autosave hook (create gating, debounce, invalid
    basket skip, 403/409 permanent stop without retry loop, seed),
    upload flow happy + too-large/PUT-failure/checksum-mismatch mapping,
    wizard step guards / submit guards / restore (draft, non-draft
    read-only, 404), and every BFF route incl. verbatim-4xx passthrough.
    `pnpm lint` clean; `next build` green (`/app/requisitions/new`
    44.6 kB client bundle, all six BFF routes present).
- **Live browser proof (compose stack rebuilt with this branch; web via
  `pnpm dev`; demo users per onboarding §5.1):**
  - Step guard live: "Lanjut ke item" disabled until a currency is chosen.
  - Choose IDR → autosave created draft → URL became
    `…/new?draft=92ef68d2-…`, status "Draf tersimpan.", currency locked.
  - Item row filled → PUT landed server-side (gateway GET: PR-000002,
    items 1, total 15,000,000).
  - **Attachment from the browser:** `browser-proof.pdf` (105 bytes) via
    the wizard input → attachment rendered **ready**; gateway detail
    embeds it (`entity_type: requisition`, `status: ready`). The presigned
    PUT reached MinIO from the page origin `localhost:3000` — CORS
    preflight included — no compose/infra changes were needed.
  - **Reload with `?draft=`:** currency IDR locked, item row, and the
    attachment list all restored from the single GET.
  - Review (total "15,000,000 IDR", attachments 1) → submit →
    "Permintaan terkirim: PR-000002"; server: `status: submitted`,
    one pending approver step, attachment ready. Reload after submit shows
    the read-only notice with autosave stopped.

## Follow-ups

- Complete/download on requisition attachments bind by entity id only
  (vendor-route parity); a same-tenant requester who learns an attachment
  id could complete/download it. Same accepted residual as the vendor
  routes; tighten together if desired.
- `docs/rbac.md`'s markdown table has a pre-existing column misalignment
  for `vendor:write` (✓ shown under requester; the code matrix + test say
  buyer) — one-cell doc fix, out of #33's scope.
- The requisitions list "New requisition" link and the wizard chrome are
  intentionally plain; a shared Stepper in `packages/ui` can absorb the
  step rail when a second wizard arrives.
- IAB cannot drive native file choosers; the Playwright e2e suite (#51+)
  should replay this upload flow with a real chooser.
