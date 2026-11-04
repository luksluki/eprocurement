# 034 — Requisition detail with timeline and actions

- **Issue:** #34
- **Branch:** feature/34-pr-detail-timeline-actions
- **Date:** 2026-10-27

## What

- **`/app/requisitions/[id]` page** (new, server component, `requireSession`
  with the exact pathname): the PRD §11 detail screen from the existing
  `GET /api/v1/requisitions/:id` aggregate (#33) — header (number, status
  badge, requester, currency, money-formatted total, submitted/created),
  items table with per-line totals (qty × unit price), approval steps table
  (step_no, role_required, threshold, status, decided_by/at, reason),
  attachments list, and the shared **#43 Timeline** (`entityType=
  "requisition"`, 2 s polling) as the first host per dev-note 043. 404 from
  the gateway renders a not-found state (other failures an error state)
  instead of crashing.
- **`lib/approval-eligibility.ts`** (new): the pure gating helper behind AC
  "actions hidden when not permitted". `decidableStep({roles, userId,
  requisition, steps})` mirrors procurement's gates in the server's order
  (services/procurement/approval/approval.go:259-281): approval:decide
  holder (approver/admin per internal/platform/rbac) → status `submitted` →
  self-approval forbidden (admins included) → the pending step with the
  smallest step_no exists → the viewer holds its role_required (admin
  exempt). Returns the step (for the buttons) or null (hidden).
- **`ApprovalActions.tsx`** (new, client): reason textarea + Approve/Reject.
  Client validation mirrors the edge contract (reject requires a non-blank
  reason ≤ 1000 chars — services/gateway/server/approvals.go:52-69); a fresh
  `Idempotency-Key` (`crypto.randomUUID`) per attempt makes double-clicks
  replay instead of double-decide; success triggers `router.refresh()` so
  the server-rendered header/steps re-render; 401 runs the refresh dance;
  4xx machine codes map to localized messages (`approval_step_not_current`
  for the lost race, …) with a generic fallback.
- **BFF `POST /api/approvals/[stepId]/decide`** (new) + `decideApprovalStep`
  / `ApprovalStep` in `lib/gateway.ts`: the submit-route conventions —
  cookie auth, Idempotency-Key length check, gateway 4xx verbatim with the
  machine code, 401 passthrough for the refresh dance, 502 otherwise.
  The decision value is validated at the BFF too (mirror of the edge).
- **Print view** (AC "print-friendly"): a `PrintButton` (window.print) plus
  Tailwind `print:hidden` on the shell's sidebar/top bar (layout), the
  actions block, the print button itself, and the timeline wrapper — the
  printed page is the document (header, items, steps). No print CSS existed
  before (grep confirmed).
- **i18n**: new dictionary sections `timeline` (the TimelineCopy contract —
  passed straight through to the component) and `requisitionDetail` (fields,
  items, steps, attachments, actions incl. the machine-code message table),
  ID + EN. The requisitions list's number column now links to the detail.

## Decisions

- **Eligibility is computed server-side and passed as data** — the page runs
  the pure helper on the claims + aggregate and hands `ApprovalActions` the
  decidable step or null; the component returns null for null. Hiding is
  data-driven (not CSS), testable at both layers, and the server keeps
  enforcing every gate — the UI helper only decides rendering.
- **The helper mirrors the server gates in the server's order** so a hidden
  case can never render and a rendered case can only fail by race, never by
  policy. Admin exemption from role_required mirrors `rbac.Check`
  (docs/rbac.md line 33: "R + approver, or an admin").
- **Money formatting: `maximumFractionDigits: 0`** — parity with the list
  (RequisitionsTable.tsx); IDR amounts are whole by seed convention and line
  totals sum exactly.
- **Print via `print:` variants, not a globals.css block**: the chrome lives
  in the shell layout, so `print:hidden` there (two words of className) is
  the smallest change; the Timeline component stays untouched (#43's file) —
  its *wrapper* hides it in print, which also covers the filter/load-more
  controls.
- **Collateral lint fix (3 `<a>` → `<Link>`)** in RequisitionWizard.tsx and
  the requisitions page: adding the dynamic `[id]` route made Next's
  `no-html-link-for-pages` rule start firing on those pre-existing anchors
  (verified: `pnpm lint` is clean with the `[id]` directory removed and
  fails with it present). Behavior-identical conversions, required to keep
  the lint gate green — flagged here because the files are otherwise
  untouched.
- **Live-proof environment repair (no code change):** the compose stack was
  running stale images — the worker predated the #94/#96 merges (its
  publisher envelopes lacked `entity_type`/`entity_id`, so every real event
  was dead-lettered by #47's consumer with `events_event_incomplete`), and
  gateway/documents predated #98 (no `/api/v1/events` route). Rebuilt
  `worker`, `gateway`, `documents` from the current tree
  (`docker compose up -d --build …`); the pipeline then worked end-to-end.

## Evidence

All commands run on this branch against the compose stack (rebuilt worker/
gateway/documents; procurement, identity, mongo, rabbitmq, postgres, redis,
minio unchanged), 2026-10-27.

- `pnpm test` — **27 files / 190 tests green** (+ assert-ui-css ok). New:
  `approval-eligibility.test.ts` (21: table of hidden cases — viewer,
  requester-only, self, non-submitted, no pending steps, role mismatch,
  non-current step; shown cases — approver at current step, admin exempt,
  multi-role holder, next step after earlier approvals),
  `api/approvals/[stepId]/decide/route.test.ts` (6: 401 without cookie and
  no gateway call, decision/reason/idempotency-key forwarding with bearer,
  local decision validation, 409 machine-code passthrough, 401 passthrough,
  502 on 5xx/network), `ApprovalActions.test.tsx` (8: render when given a
  step, null renders nothing, reject-without-reason stays local, approve
  POSTs with a UUID idempotency key then refreshes, trimmed reject reason
  forwarded, mapped 409 message, generic fallback, 401 refresh dance).
  Timeline's own tests (043) untouched.
- `pnpm lint` — clean (after the `<a>` → `<Link>` collateral above);
  `pnpm build` — green; routes present: `/app/requisitions/[id]` (3.27 kB
  client), `/api/approvals/[stepId]/decide`.
- `go vet ./... && go build ./... && go test ./...` — clean (no Go changes
  on this branch; DB-gated suites skip as designed).
- **Live 2 s end-to-end proof (decide → outbox → RabbitMQ → worker → Mongo
  → timeline), the debt from the #43 maintainer ruling:**
  - Setup on tenant `11111111-…`: user `approver@acme.example` (role
    approver, via `cmd/useradmin`), a one-step IDR rule (admin-only API),
    submitted PRs as `requester@acme.example`.
  - **Data path, API level** (decide as approver, poll
    `GET /api/v1/events?entityType=requisition&entityId=…` at 50–100 ms):
    with tick-phase decorrelation, decide → event visible in
    **163 / 329 / 650 / 962 / 968 ms** (n=5); a first correlated batch
    measured 1780/1884/2001 ms. Attribution from the outbox rows:
    `published_at - created_at` spans **135–1966 ms** — uniform under the
    2 s publisher poll (PRD §16 pins `WORKER_POLL_INTERVAL` 2 s); consumer →
    Mongo → queryable adds ~10–30 ms (worker "delivery settled … applied"
    logs at publish+5 ms). So the FR-11 budget holds at the data layer:
    the event is queryable ≤ 2 s after the decide, dominated by the
    PRD-pinned publisher poll.
  - **UI path, real browser** (in-app browser against `next dev` and a
    production `next start`): as the **requester** the detail page renders
    header/items/steps/timeline with **no approve/reject buttons**
    (AC "actions hidden" live); as the **approver** the actions render.
    Clicking *Setujui* → the timeline paints `pr.step_approved` +
    `pr.approved` newest-first and the page re-renders via `router.refresh()`
    (status badge `approved`, step decided_by/at filled, actions gone).
    Click → painted measured **5144 ms** (dev), **4748 ms** (dev,
    instrumented), **5425 ms** (production build). Decomposition
    (instrumented fetch log): publisher poll ≤ 2 s + the Timeline's 2 s
    poll interval + render; the raw `/api/events` round trip in-page is
    7–21 ms, but poll-to-poll gaps measured ≈ 3.0 s in this automation
    webview (`visibilityState: visible`, `hasFocus: false` — timer
    throttling of the unfocused view). **The honest verdict:** the painted
    timeline is the sum of two serial 2 s pollers by design (publisher +
    UI), so a strict ≤ 2 s click-to-paint cannot be guaranteed by the
    current architecture; the ≤ 2 s property is met by the data path
    (state change → event queryable), and the UI adds its documented ≤ 2 s
    poll (FR-11/#43). Follow-ups below.
  - Pipeline sanity from this proof: `pr.submitted` events are in the
    outbox (11 rows observed); `pr.created` is not (0 rows) — the create
    path remains unwired (dev-note 031 follow-up).

## Follow-ups

- **Painted-timeline latency vs FR-11 wording** — maintainer decision
  wanted: accept "data path ≤ 2 s + UI poll ≤ 2 s" as the FR-11 reading
  (both halves now demonstrated live), or tighten (shorter UI interval on
  detail pages, post-decide query invalidation, or push via SSE/WS).
- **Stale compose images were load-bearing**: the stack had been up for 2
  days running pre-#94/#96/#98 images, silently dead-lettering every real
  event (367 messages now sit in `procurement.dlq`, visible by design —
  purge at will) and 404ing `/api/v1/events`. Consider a doc/CI nudge to
  rebuild images after merging gateway/worker changes.
- Three PRs decided while the stale worker still published
  (PR-000002…000004) have their decision events dead-lettered — their
  timelines show only `pr.submitted`. Replaying from the DLQ or re-deciding
  is not possible for them (already terminal); harmless demo rows.
- Demo data added to the local stack: `approver@acme.example` (password
  `demo-password`), the IDR approval rule on tenant `11111111-…`, and
  PR-000001…PR-000018. Useful for the approvals-queue issue; wipe with a
  reset if unwanted.
- Requester/decided_by render as raw UUIDs (no user directory lookup yet —
  list-page parity); a future lookup could show emails.
- `pr.created` outbox wiring (with the tenant+entity index, dev-note 031)
  belongs to a backend issue, not #34.
