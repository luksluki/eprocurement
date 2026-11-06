# Dev note — #64 Demo video (2:38 cut, EN narration)

- **Issue:** #64 (epic #12, milestone M5, `area: demo`)
- **Branch:** `feature/64-demo-video-en-subtitles`
- **Date:** 2026-11-06
- **Status:** recorded; upload pending

## What

The video is recorded and measured. This note replaces the earlier 5:35
production plan with the cut that actually exists.

| Item | Value |
|---|---|
| Duration | **157.88 s** (2:37.9), `ffprobe` |
| Resolution | 1920×1080, 30 fps |
| Video | H.264, CRF 20 |
| Audio | AAC mono, TTS narration (English) |
| Captions | Burned in; `docs/demo/demo-captions.en.srt` has 32 cues, last at 02:37.44 |
| Size | 5.64 MB |

Files updated in this commit:

- `docs/demo/video-script.md` — rewritten for the 2:38 cut: method, timeline,
  narration as recorded, chapter list, upload checklist, known gaps.
- `docs/demo/demo-captions.en.srt` — replaced: 32 cues matching the recorded
  narration, boundaries snapped to detected pauses.
- `docs/demo/capture-runbook.md` — unchanged: the Playwright capture profile
  and the pitfalls that were hit (port 3000, `/login` health check, stale build).
- `README.md` — status line and demo block updated: video recorded, awaiting
  upload.

## Scope decision

The original plan budgeted 5:35 across 10 chapters, including terminal steps,
the MinIO console, and a resilience chapter. This cut covers the application
flow end to end plus Grafana and Jaeger in 2:38. The dropped segments are
covered by `make e2e`, `make test-p2p`, and `docs/demo-script.md`.

This is deliberate. The issue's acceptance criteria still hold: under six
minutes, audible narration, accurate captions, chapters in the description.

## Evidence

- `ffprobe`: `format.duration = 157.880000`.
- Audio boundary checks with `volumedetect`:
  - 0.4 s speech (intro), 12.4 s tail, 13.1 s speech (flow start).
  - 116.3 s speech (Grafana), 134.6 s speech (Jaeger), 150.9 s speech (closing).
  - Each boundary within 0.2 s of its chapter start.
- Caption rendering verified by the luminance delta on the lower strip:
  31.0 without subtitles, 40.65 with subtitles burned in.

## Decisions

- **TTS narration (edge-tts, `en-US-AndrewNeural`), disclosed.** No maintainer
  voice pass for this cut. The video description carries the disclosure line.
- **Burned-in captions plus a separate SRT.** Burned captions are always
  visible on muted autoplay; the SRT is uploaded for accessibility.
- **No binary in the repository.** The MP4 goes to YouTube unlisted; the repo
  keeps the production notes and the SRT.

## Pending (maintainer-manual, gates the merge)

1. Upload the MP4 as unlisted, 1080p30.
2. Paste the description block from `docs/demo/video-script.md`, including the
   chapter list and the TTS disclosure.
3. Upload `demo-captions.en.srt` as English captions; spot-check three cues.
4. Verify the chapters render in the player.
5. Final commit: write the URL and measured duration into `README.md` and this
   note, then mark the PR ready.

## Acceptance mapping

| Criterion | Status |
|---|---|
| Video under six minutes | 2:37.9 |
| Audio clear | TTS, normalised mix, no clipping |
| Captions accurate | 32 cues, pause-snapped, burned in and shipped as SRT |
| Chapters in description | 5 chapters prepared |
| TTS disclosed | Yes, in the description block and here |
