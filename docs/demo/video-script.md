# Demo video production package — #64 (EN subtitles)

Everything needed to record, edit, caption, and publish the 5-minute demo
video that [ADR-0003](../adr/0003-demo-hosting.md) makes the primary review
artifact ("a 5-minute demo video (English subtitles) recorded locally"). The
video walks the PRD [§22](../prd/prd-v0.2.md) story exactly as the presenter
runbook [`docs/demo-script.md`](../demo-script.md) (#63) stages it, and adds
one chapter the runbook does not have: the Grafana metrics view.

**Who does what.** This repo carries the *production package* (this script +
the caption file + README/runbook links + dev note). The maintainer performs
the *manual production steps* — record, edit, measure, upload — because they
need a real browser, a screen recorder, a voice, and YouTube credentials that
deliberately do not live in this repo (AGENTS.md golden rule 5). No YouTube
credentials belong in the repo or in this file, ever.

## Provenance (read before recording)

Two decisions below are **agent-applied, not maintainer-approved**: English
narration and YouTube-unlisted hosting. Both come from issue #64 comment 2
("Decisions applied by the agent … maintainer override welcome"), comment 1
parked the same two questions for the maintainer, and as of 2026-11-02 the
`in-progress` label is still on the issue — the maintainer go-signal per the
repo convention (dev-notes 054/051) has not fired. Both are flagged for
sign-off in the PR body. **If the maintainer overrides the narration
language**, only §Narration script and `demo-captions.en.srt` are reworked —
the chapter plan, timestamps, YouTube block, and README links do not change.

## Production method (decided before recording)

| Decision | Choice | Why | Documented alternative |
|---|---|---|---|
| Capture | **Human-driven screen recording** (OBS or equivalent), real browser at 1920×1080 @ 30 fps | The two attachment uploads (PR step 4, invoice step 4.2) need a real file chooser — the embedded/automated browser cannot drive one (dev-note 63:96–99) — and the video also shows a terminal, the MinIO console, Jaeger, and Grafana, i.e. surfaces outside any single browser context | Playwright `video: 'on'` per browser chapter + ffmpeg for terminal consoles; uploads driven through the page-context declare→presigned-PUT→complete pipeline (the dev-note 63 method). If used, the dev note must disclose it as the capture method |
| Audio | **Maintainer's voice**, English, recorded as a separate narration pass after the edit is assembled to the chapter timeline | Cleanest audio (no keyboard/click noise — the "audio clear" AC), retakes without re-clicking; the determinism contract makes the screen retake-identical anyway | TTS is acceptable **only if disclosed in the dev note** as the audio source; the maintainer may also override to Bahasa Indonesia narration (see Provenance) |
| Export | H.264 MP4, 1080p30, AAC audio; captions burned **out** (YouTube sidecar, not baked in) | YouTube chapter + caption features both need a clean video track | GitHub Release asset fallback (§Fallback hosting) |

## Chapter plan and time budget

Target total **5:35 (335 s)** — inside the 5:00–5:45 goal, under the 6:00
acceptance ceiling. Raw runbook material is ≈7:15 (75+60+90+75+60+45+30 s)
plus ≈25 s of Grafana; the edit recovers ≈1:50–2:45 by jump-cutting dead
waits (the 2 s timeline poll, upload→`ready` waits, wizard typing) and
pre-pasting every terminal command. Each chapter budget below has explicit
sub-items — including the three non-UI steps that must appear in the video:
the vendor honesty note, the convert-to-PO curl block, and the
match-settings curl.

| # | Chapter (YouTube) | Sub-item | Seconds |
|---|---|---|---|
| 1 | 0:00 Login & English locale | cold open on the app; state the one-tenant story | 5 |
| | | sign in as `demo-requester@story.example` | 10 |
| | | click **EN** in the header (runbook step 0) | 5 |
| 2 | 0:20 Requisition with attachment | wizard **Details** (IDR) | 6 |
| | | wizard **Items** — `Demo story laptop`, qty 2, price `7500000` (cents), category `it`; "Draft saved." badge | 14 |
| | | wizard **Attachments** — upload `e2e/fixtures/invoice-attachment.png`; narrate the draft-only window | 12 |
| | | **Review** total 15,000,000 cents → submit → **PR-000001** `submitted` | 8 |
| | | **non-UI (a): vendor honesty note** — `/app/vendors` read-only list (PT Demo Makmur, `active`), pre-created by `make demo`, create-UI does not exist (runbook 95–98) | 10 |
| 3 | 1:10 Approval & event timeline | sign out / sign in as `demo-approver@story.example` | 8 |
| | | note + **Approve** → badge `approved` | 10 |
| | | **Timeline**: `pr.approved`, `pr.step_approved`, `pr.submitted` (jump-cut the 2 s poll) | 12 |
| 4 | 1:40 PO: convert (API), issue, acknowledge | **non-UI (b): convert curl block** — the runbook's three pre-pasted commands (login → `TOKEN`, vendor lookup → `VENDOR_ID`, `POST …/from-requisition/<id>`) → `"number":"PO-000001"` (runbook 115–132) | 20 |
| | | UI: PR-000001 `closed`; **Purchase orders** → PO-000001 (draft) | 5 |
| | | **Issue** → `issued` | 8 |
| | | **Acknowledge vendor** → `acknowledged` | 7 |
| | | narration bridge: all three wrote outbox events | 5 |
| 5 | 2:25 Partial receipt; over-receipt rejected | partial GRN qty 1 → banner **GRN-000001**, items 2/1/1, PO `partially_received` | 18 |
| | | over-receipt attempt (2 vs remaining 1) → inline rejection, nothing submitted | 12 |
| | | name the server backstop: `409 grn_over_receipt` | 5 |
| 6 | 3:00 Invoice, attachment gate, 3-way match | **Create invoice** `INV-DEMO-001`, qty 1 at PO price → `pending`, total IDR 75,000 | 12 |
| | | upload attachment → wait for `ready` (jump-cut); **Run match** disabled → enabled | 13 |
| | | **Run match** → verdict **Matched**, line 2/1/1, Δ 0 / IDR 0 | 12 |
| | | **non-UI (c): tolerance config** — pre-pasted `curl GET /api/v1/match-settings` → `qty_pct: 5`, `price_pct: 2` (runbook 190–198) | 8 |
| 7 | 3:45 Dashboard, export, MinIO, job history | dashboard: spend 30/90 **IDR 150,000**, PT Demo Makmur row, `partially_received: 1` | 15 |
| | | **Reports** empty state → **Run export now** → one `success`+`manual` row, Rows 1 | 10 |
| | | MinIO console `exports/55555555-…/<today>/` object (promise the pattern, never a fixed filename) + **Download CSV** → one data row | 15 |
| 8 | 4:25 Resilience: worker stop/start | `docker compose stop worker` (pre-typed) | 4 |
| | | single trigger via pre-pasted `curl -i -X POST localhost:8080/api/v1/exports` → **`202` + `"status":"requested"`**; run history does not move | 8 |
| | | hold on the unchanged history (nothing lost, nothing processed) | 4 |
| | | `docker compose start worker` → backlog drains | 6 |
| | | the queued run appears **exactly once** (fresh `manual` row) | 8 |
| 9 | 4:55 Trace: Jaeger | Jaeger `:16686` → service `gateway` → operation `/procurement.v1.Procurement/IssuePurchaseOrder` → trace waterfall gateway → procurement → outbox publish → RabbitMQ → worker → documents | 20 |
| 10 | 5:15 Metrics: Grafana | Grafana `:3001` → dashboard **eProcurement overview** (`eproc-overview`): **Outbox lag** panel (stale while the worker was down — nothing to scrape; reads the backlog age at restart, then 0 as it drains) and **DLQ depth** panel | 20 |
| | | **Total** | **335** |

Chapter constraints: first chapter at 0:00 ✓, 10 chapters (≥3) ✓, shortest
chapter 20 s (≥10 s) ✓, total 5:35 (<6:00) ✓.

**Budget validation against the measured cut (fill in before upload).** The
table above is the plan; the acceptance criterion is measured on the final
cut. After editing, measure with:

```bash
ffprobe -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 demo-video.mp4
```

Record the number here, in the dev note, and (if it differs from plan by more
than ±2 s per boundary) update the YouTube chapter timestamps and the SRT to
the measured boundaries **before** upload:

- Measured total: ______ (target 335 s, hard ceiling <360 s)

Do not promise a specific DLQ depth on camera: the dev broker may still carry
DLQ messages from earlier chaos testing (dev-note 055 measured 367), and
`make demo` does not reset RabbitMQ.

## Narration script (English)

The cue table below is the single source for the narration and for
[`demo-captions.en.srt`](demo-captions.en.srt) — the SRT carries exactly this
text at exactly these times. A Bahasa-Indonesia override replaces the text
inside the cues only; timecodes, chapters, and README links stay as-is.

| # | In → out | Text |
|---|---|---|
| 1 | 0:00.5 → 0:04.5 | This is eProcurement: one tenant, one procure-to-pay story, end to end. |
| 2 | 0:04.8 → 0:09.3 | Everything runs locally on Docker Compose, seeded by make demo. |
| 3 | 0:09.6 → 0:14.4 | We sign in as the requester and pin the UI to English. |
| 4 | 0:14.7 → 0:19.7 | The locale is a cookie — it survives navigation for the whole demo. |
| 5 | 0:20.2 → 0:26.2 | New requisition: a four-step wizard — details, items, attachments, review. |
| 6 | 0:26.5 → 0:33.0 | Currency IDR. One line — two laptops; the price field is cents: seven and a half million. |
| 7 | 0:33.3 → 0:38.3 | The basket autosaves as a draft — the badge confirms it. |
| 8 | 0:38.6 → 0:46.2 | Attachments go straight to MinIO on a presigned URL — and only while the requisition is still a draft. |
| 9 | 0:46.5 → 0:52.3 | Review shows the total — fifteen million cents, one hundred fifty thousand rupiah. |
| 10 | 0:52.6 → 0:58.2 | Submit. PR-000001, status submitted. |
| 11 | 0:58.5 → 1:06.5 | One honesty note: the vendor came pre-created by make demo — the vendors page is read-only, no create form yet. |
| 12 | 1:06.8 → 1:09.8 | PT Demo Makmur, active — the vendor this story uses. |
| 13 | 1:10.2 → 1:16.2 | Switch to the approver — self-approval is forbidden, even for admins. |
| 14 | 1:16.5 → 1:21.7 | The approval panel appears. Approve, with a note. |
| 15 | 1:22.0 → 1:27.8 | Status flips to approved; the step records the reason. |
| 16 | 1:28.1 → 1:39.7 | The timeline is the MongoDB event history — submitted, step approved, approved — each decision lands within seconds. |
| 17 | 1:40.2 → 1:46.4 | Now the buyer leg. Convert-to-PO has no UI yet — issue one-twelve — so it's one API call. |
| 18 | 1:46.7 → 1:53.9 | Three commands: log in, look up the vendor, create the PO from the requisition. |
| 19 | 1:54.2 → 2:00.0 | The answer: PO-000001. In the UI, the requisition is closed. |
| 20 | 2:00.3 → 2:06.0 | Issue the purchase order — issued. |
| 21 | 2:06.3 → 2:11.7 | Acknowledge the vendor — acknowledged. |
| 22 | 2:12.0 → 2:24.6 | Convert, issue and acknowledge all wrote events through the same transactional outbox — remember that for the resilience chapter. |
| 23 | 2:25.2 → 2:31.0 | Record a partial goods receipt. The line shows two remaining — receive one. |
| 24 | 2:31.3 → 2:39.3 | GRN-000001: two ordered, one received, one remaining — partially received, deliberately not received. |
| 25 | 2:39.6 → 2:46.8 | Now attempt the over-receipt: two, against a remaining of one. |
| 26 | 2:47.1 → 2:53.6 | The form refuses, inline — nothing is submitted. |
| 27 | 2:53.9 → 2:59.8 | The server holds the same guard as a typed backstop: 409, grn_over_receipt. |
| 28 | 3:00.2 → 3:06.4 | Finance. Create the invoice on the partial receipt — bill what was received. |
| 29 | 3:06.7 → 3:12.5 | INV-DEMO-001, quantity one, at the PO price — pending. |
| 30 | 3:12.8 → 3:20.3 | Upload the vendor invoice document. Until an attachment is ready, Run match stays disabled — the hard gate. |
| 31 | 3:20.6 → 3:25.2 | Run match — verdict: Matched. |
| 32 | 3:25.5 → 3:34.5 | The line reads: ordered two, received one, invoiced one — deltas zero. The match compares invoiced against received, not ordered. |
| 33 | 3:34.8 → 3:44.6 | The tolerances — five percent quantity, two percent price — are per-tenant config; today that's an API call. |
| 34 | 3:45.2 → 3:52.2 | Dashboard. Thirty-ninety-day spend: one hundred fifty thousand rupiah — the issued PO total. |
| 35 | 3:52.5 → 3:58.5 | Spend by vendor: PT Demo Makmur. PO status: partially received, one. |
| 36 | 3:58.8 → 4:04.8 | Reports. The export history starts empty — every demo reset begins here. |
| 37 | 4:05.1 → 4:10.7 | Run export now — a manual run succeeds, one row. |
| 38 | 4:11.0 → 4:17.7 | In MinIO the CSV lands under exports, tenant, today — a clock-stamped filename. |
| 39 | 4:18.0 → 4:24.6 | One data row: PO-000001, PT Demo Makmur, one hundred fifty thousand rupiah. |
| 40 | 4:25.2 → 4:29.7 | Resilience. Stop the worker. |
| 41 | 4:30.0 → 4:36.5 | Trigger the export once, from the API: 202, requested. The run history does not move. |
| 42 | 4:36.8 → 4:42.3 | Nothing is lost — it is waiting in the outbox. |
| 43 | 4:42.6 → 4:48.4 | Start the worker — the backlog drains; the queued run appears exactly once. |
| 44 | 4:48.7 → 4:54.8 | Consumers are idempotent, keyed on event id. No duplicates. |
| 45 | 4:55.2 → 5:01.4 | Tracing. In Jaeger: the gateway service, the purchase-order-issue operation. |
| 46 | 5:01.7 → 5:09.4 | One trace across gateway, procurement, the outbox publish, RabbitMQ, the worker, and the documents projection. |
| 47 | 5:09.7 → 5:14.8 | Every hop a span — the same request you just watched. |
| 48 | 5:15.2 → 5:20.4 | The same system on Grafana's eProcurement overview. |
| 49 | 5:20.7 → 5:28.7 | Outbox lag read the backlog's age when the worker restarted, then returned to zero as it drained. |
| 50 | 5:29.0 → 5:34.6 | DLQ depth: where a poison message would surface. That's the demo — thanks for watching. |

Pacing target ≈2.5 words/second; every cue sits inside its chapter window, so
chapter boundaries stay clean if a cue needs a small nudge at record time.

## Recording checklist (pre-flight)

1. `make compose-up`, then `docker compose --profile observability up -d --wait`
   (Jaeger + Prometheus + Grafana — chapters 9–10 need them; runbook pre-demo).
2. `make demo` — fresh story; keep the installer summary out of the frame or
   accept it (it contains no secrets).
3. Web app on `http://localhost:3000` (`pnpm dev` or `pnpm --filter
   @eprocurement/web start` — either; do not switch mid-recording).
4. Clean browser profile: no bookmarks bar, no extensions, no personal tabs,
   OS notifications off, zoom 100%, window ≥1920×1080.
5. Rehearse once end-to-end. Retakes are safe — the runbook's determinism
   contract (`make demo` twice → identical observables) is what makes a
   re-record cheap. Mind identity's 5 logins/min-per-email limit across
   retakes (runbook troubleshooting).
6. Terminal ready with the three curl blocks and the two `docker compose`
   commands **pre-pasted** (one per line, Enter on camera); export CSV/objects
   from rehearsals already noted as expected-orphaned (runbook: reset does
   not delete MinIO bytes).

## No-secret-in-frame review checklist (run on the edit, before export)

Review every frame class; cut or blur anything that fails:

- **UI frames** — only the demo fixture credentials
  (`demo-requester@story.example` / `demo-password`,
  `demo-approver@story.example`) may appear; they are public fixtures. No
  `.env` values, no real passwords, no personal data in the browser chrome.
- **Terminal frames** — the runbook curl blocks only. Never `cat .env`; never
  echo `$TOKEN` or scroll a raw `access_token` into view (the block assigns
  it to a variable and passes `$TOKEN` — keep it that way on camera).
- **MinIO console** — local defaults (`eprocurement` / `eprocurement`) are
  public, but stay inside `exports/55555555-…/`; do not browse other buckets.
- **Grafana** — cut the login screen; show the provisioned dashboard
  directly (the admin password comes from `.env`).
- **Jaeger** — no credentials exist; still avoid the service-list sidebar if
  it shows unrelated hostnames.

## Edit checklist

- Jump-cut the dead waits: the 2 s timeline poll, attachment upload→`ready`,
  dashboard initial load, wizard field typing (cut between fields, keep the
  "Draft saved." badge visible once).
- Terminal segments: show the pre-pasted command and its output; no live
  typing on camera.
- Keep these on screen long enough to read: `PR-000001`, `PO-000001`,
  `GRN-000001`, `INV-DEMO-001`, the `Matched` verdict, the `409
  grn_over_receipt` mention, `qty_pct: 5` / `price_pct: 2`, the export run
  row (`success`, `manual`, Rows 1).
- One resilience trigger only (the curl POST) so "exactly one run" stays
  true — do not also click the UI button in chapter 8.
- Assemble to the chapter timeline above, then measure (§ budget validation)
  and, if needed, nudge chapter timestamps + SRT to the measured boundaries.

## Upload (manual, maintainer) — order matters

**Record → edit → measure → upload → get URL → only then write the URL into
README/dev note.** The PR carries the whole package; its final commit adds
the live URL to `README.md` (demo artifacts block) and the dev note. The PR
is not merged before the URL and the measured duration are written.

1. Upload to YouTube as **unlisted** (agent-applied decision, flagged for
   sign-off).
2. Paste the description block below (chapters included).
3. YouTube Studio → Subtitles → **upload the SRT as language "English"**
   (`docs/demo/demo-captions.en.srt`). Do **not** publish auto-generated
   captions alongside it; unpublish/replace them if YouTube adds them.
4. Verify in the player: chapters render in the progress bar; EN captions
   play; spot-check 3 cues against the narration (log the check in the dev
   note).
5. Copy the video URL → final commit: README demo-artifacts line + dev-note
   evidence (URL, unlisted setting, measured duration, chapter render,
   caption check).

### YouTube description block (paste-ready)

```text
eProcurement (ELCODE ProcureKit) — procure-to-pay in one tenant: requisition with attachment, approval with event timeline, PO convert/issue/acknowledge (convert via API — UI pending #112), partial goods receipt with a rejected over-receipt, three-way invoice match, CSV export to MinIO, worker-kill resilience, then the same flow in Jaeger traces and on the Grafana dashboard.

00:00 Login & English locale
00:20 Requisition with attachment
01:10 Approval & event timeline
01:40 PO: convert (API), issue, acknowledge
02:25 Partial receipt; over-receipt rejected
03:00 Invoice, attachment gate, 3-way match
03:45 Dashboard, export, MinIO, job history
04:25 Resilience: worker stop/start
04:55 Trace: Jaeger
05:15 Metrics: Grafana

Run the story yourself: https://github.com/luksluki/eprocurement — `make demo` + docs/demo-script.md. Captions: English (committed as docs/demo/demo-captions.en.srt).
```

## Fallback hosting

If the maintainer rejects YouTube: attach the MP4 (plus
`demo-captions.en.srt`) as a **GitHub Release asset** on the repo's release,
paste the chapter list into the release notes body (YouTube auto-chapters do
not apply), and link the release from the same README line. No other step of
this package changes.
