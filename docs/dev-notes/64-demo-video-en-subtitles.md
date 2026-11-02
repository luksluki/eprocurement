# Dev note — #64 Demo video production package (EN subtitles)

- **Issue:** #64 (epic #12, milestone M5, `area: demo`)
- **Branch:** feature/64-demo-video-en-subtitles
- **Date:** 2026-11-02

> **Provenance:** both comments on issue #64 are **agent posts**, not
> maintainer answers. Comment 1 (2026-11-02) parks two questions (narration
> language; hosting); comment 2 (2026-11-02) states "Decisions applied by the
> agent … maintainer override welcome" and "Implementation may proceed on
> these bases". The `in-progress` label is **still attached** (re-checked at
> implementation start, 2026-11-02: `gh issue view 64 --json labels,comments`
> — labels `type: feature`, `area: demo`, `priority: P0`, `size: M`,
> `in-progress`; no comment after 2026-11-02 08:39Z). Per the repo's go
> convention the maintainer removes `in-progress` before implementation
> (dev-notes 054:15–18, 051:22) — that signal has **not** fired. This PR
> proceeds on the agent-applied bases because comment 2 explicitly says
> implementation may proceed, and **both decisions are flagged for maintainer
> sign-off in the PR body** (golden rule 10; the dev-note 055 precedent):
> 1. **Narration language = English** (Bahasa Indonesia override reworks
>    only the narration table + SRT — chapter plan, timestamps, YouTube
>    block, and README links are language-independent by construction).
> 2. **Hosting = YouTube unlisted** (GitHub Release asset is the documented
>    fallback in `docs/demo/video-script.md`).

## What

- **`docs/demo/video-script.md` (new)** — the production package: production
  method decided up front (human-driven OBS capture with a real browser at
  1080p30 — the two attachment uploads need a real file chooser, dev-note
  63:96–99 — with the Playwright `video: 'on'` alternative documented;
  maintainer-voice narration recorded as a post-edit pass, TTS allowed only
  if disclosed), a 10-chapter time budget totalling **5:35 (335 s)** with
  explicit sub-items for the three non-UI steps (vendor honesty note
  10 s; convert curl block 20 s; match-settings curl 8 s) and the Grafana
  chapter (20 s), the 50-cue English narration script, recording + edit +
  no-secret-in-frame checklists (UI / terminal / MinIO / Grafana / Jaeger
  frames), a ffprobe duration-measurement step with a fill-in validation
  row, the paste-ready YouTube description with chapters, upload-as-unlisted
  + upload-the-SRT-as-English steps, and the GitHub Release fallback.
- **`docs/demo/demo-captions.en.srt` (new)** — 50 English cues, exactly the
  narration table's text and timecodes, every cue inside its chapter window,
  last cue ends 5:34.6 (< 5:35 planned total).
- **`README.md`** — status line 7 (demo script merged; video package in this
  PR, link lands with the upload) and the full Demo & review artifacts block:
  #63 now "merged" with the runbook link, #64 links the production package
  with the video URL explicitly pending upload, and the stale "Until the
  script lands…" sentence (written before #63 merged in 6fda66a) is rewritten
  to point at `make demo` + the runbook, with `make e2e` / `make test-p2p`
  as the asserted equivalents.
- **`docs/demo-script.md`** — one cross-link sentence in the intro pointing
  at the video package.
- No application/infrastructure code touched (compose, Makefile, deploy/,
  scripts/ unchanged).

## Decisions

- **Production package in-repo; record/edit/upload are maintainer-manual.**
  No YouTube credentials exist in the repo (golden rule 5) and capture needs
  a human-driven real browser; the repo carries everything the maintainer
  executes. The export order is pinned: record → edit → measure → upload →
  URL → **then** the final commit writes the URL into README/dev-note; the
  PR is not merged before the URL and measured duration are written.
- **335 s target with per-chapter sub-item budgets.** Raw runbook material
  is ≈7:15 + ≈25 s Grafana; the plan compresses to 5:35 via documented cuts
  (jump-cut the 2 s timeline poll, upload→`ready` waits, wizard typing;
  pre-paste every terminal command). The budget is validated against the
  measured cut (ffprobe), not the runbook estimates; >±2 s boundary drift
  means the chapter timestamps + SRT are updated before upload.
- **Grafana chapter is in scope, Jaeger alone is not enough.** The issue's
  "trace/metrics segment" names both; the runbook (#63) only covers Jaeger
  (step 7), so the video adds chapter 10 on the provisioned
  `eprocurement-overview` dashboard (compose observability profile,
  `:3001`): Outbox lag (stale while the worker is stopped — nothing to
  scrape; reads the backlog age at restart, then 0) and DLQ depth.
- **Chapter 8 triggers resilience once, via curl.** A single pre-pasted
  `POST /api/v1/exports` makes the `202 requested` response visible on
  camera AND keeps "the queued run appears exactly once" literally true —
  adding a UI-button trigger in the same chapter would queue a second run.
- **The narration does not promise a DLQ value.** The dev broker may carry
  old DLQ messages (dev-note 055 measured 367) that `make demo` does not
  reset; cue 50 says where a poison message would surface instead of naming
  a number.
- **Over-receipt narration names the backstop, not a fake UI 409.** The UI
  path rejects inline; the `409 grn_over_receipt` is the server backstop —
  cue 27 attributes it to the server, matching the runbook's honesty.
- **Language-override isolation.** Cue timecodes and chapter boundaries are
  fixed; an Indonesian narration override rewrites only the cue text in the
  narration table and the SRT, leaving timing, chapters, and README
  untouched.

## Evidence

Commands run at implementation time (2026-11-02, repo root):

- `gh issue view 64 --json number,title,state,labels,comments` — state
  OPEN; labels include `in-progress`; exactly two comments, both authored by
  the issue-flow account, comment 2 = "Decisions applied by the agent …
  Implementation may proceed on these bases". No maintainer reply.
- Budget/chapter/SRT validation (ad-hoc Python parsing the committed files,
  not hand-counted):
  - sub-item seconds in `video-script.md` sum to **335** = the stated total;
  - YouTube chapter block: 10 chapters, first at **0:00**, shortest
    **20 s** (≥10 s), strictly ascending, last start 5:15 < 335 s total;
  - chapter boundaries recomputed from the sub-item sums
    (0:00/0:20/1:10/1:40/2:25/3:00/3:45/4:25/4:55/5:15) match the YouTube
    block verbatim;
  - `demo-captions.en.srt`: 50 cues, sequential numbering, valid
    `HH:MM:SS,mmm` stamps, monotonic and non-overlapping, cue lengths
    3.0–12.6 s, last end **334.6 s ≤ 335 s**, and every cue inside its
    chapter window.
- Link targets exist: `docs/demo/video-script.md`,
  `docs/demo/demo-captions.en.srt`, `docs/demo-script.md`,
  `docs/adr/0003-demo-hosting.md` (README + demo-script links resolve);
  Grafana dashboard panels "Outbox lag" and "DLQ depth" confirmed in
  `deploy/compose/grafana/dashboards/eprocurement-overview.json` (uid
  `eproc-overview`) and Grafana on `:3001` behind the observability profile
  (compose.yaml:125–137).
- `git status` — only the five intended paths touched (2 new docs/demo
  files, README.md, docs/demo-script.md, this note); no code/infra files.
- Gates (`go vet/build/test`, `pnpm build/lint/test`) — **not run by me**
  (no Go/TypeScript touched); the workflow script runs the full gate set
  after this note, per the ask (the dev-note 055:239–240 disclosure
  pattern).

**Pending (maintainer-manual, gates the merge):** record + edit to the
chapter timeline; measure with ffprobe and fill the validation row above;
upload unlisted with the description block; upload
`docs/demo/demo-captions.en.srt` as English captions and spot-check ≥3 cues;
verify chapters render; then the final commit writes the URL + measured
duration into README and this note. None of these can be executed from the
repo — they are the PR's explicit handoff.

## Follow-ups

- **PR body must carry the sign-off items** (verification list): (a)
  narration language EN (agent-applied); (b) YouTube unlisted hosting
  (agent-applied); (c) the pending URL/duration final commit as a merge
  condition.
- **#112** (convert-to-PO UI) — when it lands, chapter 4 loses its terminal
  detour and the curl sub-item goes away (same follow-up as dev-note 63).
- **#65** (tunnel runbook) — the last open M5 demo artifact after this PR.
- If the maintainer records narration in Bahasa Indonesia, rework the
  narration table + SRT only (see Provenance); no timing/chapter changes.
