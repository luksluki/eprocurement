# Demo script — the 5-minute procure-to-pay story (PRD §22)

This is the presenter runbook for the PRD §22 demo: one tenant, four role
narratives (requester → approver → buyer → finance), a partial goods
receipt with a rejected over-receipt attempt, a three-way invoice match,
a CSV export with visible job history, and the resilience and tracing
closers. Every "Expected" line below was recorded from a clean run of the
click path against the compose stack — where a value cannot be identical
between runs (UUIDs, timestamps, export filenames), the doc says so and
gives the pattern instead of a fixed value.

The seeded story is installed by [`make demo`](#0-pre-demo-once) — it
resets the demo tenant and (re)installs the fixtures idempotently, so
running it twice produces the same story. The acceptance test for this
script is exactly that: two runs, identical observable results (see the
[determinism contract](#determinism-contract)).

## 0. Pre-demo (once)

```bash
make compose-up                                   # the core stack, healthy
docker compose --profile observability up -d --wait   # Jaeger etc. — steps 6–7 only
cp apps/web/.env.example apps/web/.env.local      # then edit if your ports differ
pnpm dev                                          # web app on http://localhost:3000
make demo                                         # reset demo tenant + install fixtures
```

Why the extra observability command: `make compose-up` starts the default
profile only; Prometheus/Grafana/Jaeger live behind the opt-in
`observability` profile (compose.yaml), and nothing sets `COMPOSE_PROFILES`
by default. Run it when you plan to show steps 6–7; skip it for a short
rehearsal.

Presenting to remote viewers? Expose this local stack through a
Cloudflare Tunnel for the demo window — see the
[live-demo tunnel runbook](ops/live-demo-tunnel.md) (only the web UI on
`:3000` goes through the tunnel).

What `make demo` installs (its own summary output — keep it, it is part of
the expected result):

| Fixture | Value |
|---|---|
| Tenant | `55555555-5555-5555-5555-555555555555` |
| Requester | `demo-requester@story.example` / `demo-password` (admin role) |
| Approver | `demo-approver@story.example` / `demo-password` (admin role) |
| Vendor | `DEMO-VND-01` — PT Demo Makmur (active) |
| Approval chain | 1 step, IDR, role `approver`, threshold 0 |
| Match tolerance | qty 5% / price 2% (non-default, so the config step shows real values) |
| Next numbers | PR-000001 / PO-000001 / GRN-000001 (counters reset) |

Two fixture users exist because self-approval is forbidden for everyone,
admins included (`docs/rbac.md`) — the requester can never approve their
own PR, so the story needs a second login. Both hold the admin role, which
carries every permission; the PRD §22 "Buyer" and "Finance" legs run under
those same two logins (narrate the role, not a third account — that keeps
the demo inside identity's 5 logins/min-per-email budget, the onboarding
§5.1 trap).

## The story

### Step 0 — Pin the locale to EN (before the clock)

Open <http://localhost:3000>, sign in as
`demo-requester@story.example` / `demo-password` (**Sign in**). In the
header, click **EN** in the language switcher.

**Expected:** the whole UI relabels to English. The app's default locale
is `id` (PRD §19 ordering); the demo runs in EN so the recorded video
matches ADR-0003's "English subtitles" framing and — critically for
expected output below — the match verdict renders as **"Matched"**, not
"Cocok". The choice is a cookie (`eproc_locale`), so it survives
navigation but not a browser-profile wipe; check the switcher if copy
looks Indonesian.

### Step 1 — Requester: PR with attachment (≈75 s)

1. **Requisitions → New requisition.** The 4-step wizard shows
   **Details → Items → Attachments → Review**.
2. **Details:** currency **IDR** → **Next**.
3. **Items:** add one line — description `Demo story laptop`, qty `2`,
   unit price `7500000` (the field is **cents** — IDR 75,000), category
   `it` → **Next**. (The basket autosaves debounced; the "Draft saved."
   badge confirms it.)
4. **Attachments:** upload any allowed file (pdf, png, jpeg, webp, docx,
   xlsx; ≤ 25 MB) — the repo ships a valid one at
   `e2e/fixtures/invoice-attachment.png`. The upload goes straight to
   MinIO via a presigned URL. Attachments are a **draft-only window** —
   after submitting, the requisition can take no more files, so do it in
   this step.
5. **Review:** the total reads **"Total (cents): 15,000,000 IDR"**
   (2 × 7,500,000 cents; the detail page will show it as IDR 150,000) →
   **Submit for approval**.

**Expected:** the success panel appears; the list/detail shows number
**PR-000001** with status badge `submitted`, the attachment listed, and
the requester's own detail view has **no approval buttons** (self-approval
forbidden — worth saying out loud).

*Honest gap:* the vendor in this story was **pre-created by `make demo`**,
not typed in the UI — `/app/vendors` is a read-only list with no create
form. Show the list (one row: PT Demo Makmur, `active`) when narrating
"create vendor" from the PRD; the create-vendor UI does not exist yet.

### Step 2 — Approver: approve with note, show the timeline (≈60 s)

1. **Sign out** (header) → sign in as `demo-approver@story.example`.
2. **Approvals** (or open PR-000001 from **Requisitions**): the approval
   panel is visible now. Type a note ("demo approval") → **Approve**.

**Expected:** status badge flips `submitted` → `approved`; the approval
steps table shows the step `approved` with your reason; the **Timeline**
section below (MongoDB event history, 2 s polling) gains the decision
events within seconds — newest first: `pr.approved`, `pr.step_approved`,
`pr.submitted`, each with actor and timestamp. Optional: **Audit Log** in
the sidebar shows the same decision with actor IP and trace id.

### Step 3 — Buyer: convert → issue → acknowledge → PARTIAL receipt (≈90 s)

1. **Convert (terminal, one command — no UI yet, issue #112):** copy the
   requisition id from the URL (`/app/requisitions/<uuid>`), then:

   ```bash
   TOKEN=$(curl -s -X POST localhost:8080/api/v1/auth/login \
     -H 'content-type: application/json' \
     -d '{"email":"demo-approver@story.example","password":"demo-password"}' \
     | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
   VENDOR_ID=$(curl -s "localhost:8080/api/v1/vendors?q=DEMO-VND-01&page_size=50" \
     -H "authorization: Bearer $TOKEN" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
   curl -s -X POST "localhost:8080/api/v1/purchase-orders/from-requisition/<REQUISITION_ID>" \
     -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' \
     -d "{\"vendor_id\":\"$VENDOR_ID\"}"
   ```

   **Expected:** the answer contains `"number":"PO-000001"`. Back in the
   UI, PR-000001 now shows status `closed`, and **Purchase orders** lists
   **PO-000001** (draft).

2. Open **PO-000001** → **Issue** → status `issued` → **Acknowledge
   vendor** → status `acknowledged`.

3. **Record a PARTIAL receipt:** in the goods receipt form the line shows
   its remaining quantity `(2)`. Enter `1` → **Record receipt**.

   **Expected:** banner **"Receipt recorded: GRN-000001"**; the items
   table shows Ordered 2 / Received 1 / Remaining 1; the PO status badge
   is **`partially_received`** — deliberately *not* `received` (1 of 2
   still open; PRD §22 pins this exact shape).

4. **Attempt the over-receipt:** in the still-visible receipt form the
   line's remaining is now `(1)`. Type `2` → **Record receipt**.

   **Expected:** the form refuses, nothing is submitted — the line shows
   the inline red **"Quantity exceeds this line's remaining 1."** and the
   form-level **"Fill at least one line with a quantity greater than 0."**
   (the invalid line was dropped from the submission set). PO-000001
   stays `partially_received` with exactly one GRN. The form is the UI
   guard; the server's typed backstop fires if a request bypasses it —
   `409`, code `grn_over_receipt`, body detail
   `grn_over_receipt: po_item=… received=1 requested=2 ordered=2`
   (cumulative received vs ordered, `docs/schema/procurement.md`).

### Step 4 — Finance: invoice on the partial receipt → 3-way match (≈75 s)

1. On PO-000001: **Create invoice**. Number **`INV-DEMO-001`**, qty `1`
   (bill what was received) at the prefilled PO price → **Save invoice**.

   **Expected:** invoice detail shows number INV-DEMO-001, status badge
   `pending`, total **IDR 75,000**, and the billed-PO link. Creating an
   invoice on a `partially_received` PO is legal — invoicable statuses
   are `acknowledged`, `partially_received`, `received`.

2. **Upload the vendor invoice document** (the panel below the lines; a
   valid pdf/png/jpeg/webp/docx/xlsx — `e2e/fixtures/invoice-attachment.png`
   again). Wait until the attachment row reads **`ready`**.

   **Expected:** while no attachment is `ready` the **Run match** button
   stays disabled with the note "Upload and complete a vendor invoice
   attachment (ready status) before matching." — the hard attachment
   gate. It enables exactly when the row flips to `ready`.

3. **Run match**.

   **Expected:** the **Three-way match result** panel appears with verdict
   badge **"Matched"** (EN locale — step 0) and the header status badge
   flips `pending` → `matched`. The per-line row — the discrepancy detail
   surface — reads: PO line (the item uuid), Ordered `2`, Received `1`,
   Invoiced `1`, Δ qty `0`, PO price `IDR 75,000`, Invoice price
   `IDR 75,000`, Δ price `IDR 0`, a single green `ok` flag pill, and an
   empty reason (`—`). Point at it while explaining the guard: the match
   compares **invoiced vs received** (not ordered), so billing exactly
   the partial receipt is a match. Status badges show the raw enum
   (`matched`, not "Matched") — the badge is the wire status.

4. **Tolerance config (terminal — no UI page yet):**

   ```bash
   curl -s localhost:8080/api/v1/match-settings -H "authorization: Bearer $TOKEN"
   ```

   **Expected:** `qty_pct: 5` and `price_pct: 2` — the row `make demo`
   installed (the effective default with no row stored is 0%/0%). The
   admin settings page that would host this is still a placeholder.

### Step 5 — Dashboard, export, MinIO, job history (≈60 s)

1. **Dashboard.**

   **Expected:** the KPI cards show **spend 30/90 days = IDR 150,000**
   (the issued PO total — the spend basis is POs with `issued_at` in the
   window, drafts and cancelled excluded), and the **spend by vendor**
   table has one row: PT Demo Makmur, IDR 150,000. The **PO status
   distribution** shows `partially_received: 1`.

2. **Reports** → the page opens on the empty state ("No export runs yet.
   The first nightly export runs on the next schedule.") → **Run export
   now** (admin-only button).

   **Expected:** within a few seconds one run row appears — Scheduled
   (timestamp), Status `success` + `manual` badge, **Rows `1`**, and a
   **Download CSV** button. The run history is MongoDB `export_runs`
   projected live; `make demo` resets it, so every run of the demo starts
   from this empty state.

3. Open the **MinIO console** <http://localhost:9001>
   (eprocurement / eprocurement) → bucket → `exports/55555555-…/<today>/`.

   **Expected:** one CSV object named
   **`purchase-orders-manual-{HHMMSS}.csv`** — the clock segment differs
   on every run by design (it keeps manual runs off the nightly artifact),
   so promise the **pattern**, never a fixed filename. Click **Download
   CSV** on the run row instead — the presigned link always matches. The
   CSV holds a header plus exactly one data row:
   `PO-000001,DEMO-VND-01,PT Demo Makmur,IDR,15000000,partially_received,…`.

### Step 6 — Resilience: kill the worker (≈45 s)

```bash
docker compose stop worker
```

In the UI, **Reports → Run export now** again. The POST succeeds
immediately — `202 {"event_id":…,"status":"requested"}` — because the
`export.requested` outbox row is committed with the business write; the
run history deliberately does NOT move yet (nothing is lost, nothing
processed).

```bash
docker compose start worker && docker compose logs -f worker
```

**Expected:** the worker drains the backlog on restart — the queued run
appears in the history exactly once (a `manual` row with a fresh
timestamp), no duplicates: idempotent consumers keyed on `event_id`
(`docs/prd/prd-v0.2.md` §14.5). This is also why `make demo` resets
`export_runs`: manual runs upsert on `{tenant_id, scheduled_for}` (the
event's occurred_at), so without the reset every demo run would stack one
more history row.

### Step 7 — Observability: the PO issue trace (≈30 s)

Open **Jaeger** <http://localhost:16686> → Service = `gateway` →
Operation = `/procurement.v1.Procurement/IssuePurchaseOrder` → **Find
traces** → open the trace from the demo (or follow a `trace_id` from the
**Audit Log** page).

**Expected:** one trace spanning gateway → procurement (gRPC) → the
outbox publish → RabbitMQ → worker → documents/MongoDB projection, with
spans per hop (the worker's spans name the outbox event, e.g.
`outbox publish po.acknowledged`). This needs the observability profile
from pre-demo; without it the collector has nowhere to export to.

## Determinism contract

Run `make demo` twice from any state and walk the story twice — the
observable results are identical in exactly this sense:

| Identical across runs | Why |
|---|---|
| Numbers PR-000001 / PO-000001 / GRN-000001 | `make demo` resets the three per-tenant counters with the rows |
| INV-DEMO-001 accepted | invoice numbers are unique per (tenant, vendor, number); reset frees it |
| Status sequence draft→submitted→approved→closed; PO issued→acknowledged→partially_received | same click path, same rules |
| Verdict "Matched", line 2/1/1, Δ 0/0 | qty 5%/price 2% reinstalled identically |
| Install summary block of `make demo` | fixed fixtures, idempotent install |

| Deliberately different | Pattern |
|---|---|
| Row UUIDs (requisition/PO/GRN/invoice ids in URLs) | fresh `gen_random_uuid()` per run |
| Timestamps (timeline, audit, run history) | wall clock |
| Export filename `purchase-orders-manual-{HHMMSS}.csv` | date + clock at trigger time |

Also identical by construction: the onboarding tenants (`1111…`/`2222…`)
and the e2e tenants (`3333…`/`4444…`) are never touched — the reset is
scoped to the demo tenant id and the two demo emails.

## What the reset does not do

`make demo-reset` deletes the demo tenant's PostgreSQL rows (counters
included), its `identity.users` (by email), and its five MongoDB
collections (`events`, `audit_log`, `document_meta`, `notifications`,
`export_runs`). It does **not** delete MinIO object bytes: past
attachments and export CSVs remain as unreachable orphans (their metadata
is gone, and the pending-reaper only reaps pending declarations). For a
rehearsal-heavy laptop that is harmless clutter; `make reset` (drops all
volumes) is the nuclear option when it matters.

## Troubleshooting

- **Login 429 after repeated rehearsals** — identity throttles 5
  logins/min per email (onboarding §5.1). Wait a minute, or set
  `IDENTITY_LOGIN_RATE_PER_MIN=60` in `.env` and `make compose-up`.
- **Buttons greyed out / copy in Indonesian** — re-check step 0 (locale
  cookie) and that you are signed in as the right demo user.
- **`Run export now` missing** — the trigger is admin-only; use a demo
  (admin) login.
- **Jaeger empty** — the observability profile was not started; run the
  pre-demo command, then retry the traced request.
