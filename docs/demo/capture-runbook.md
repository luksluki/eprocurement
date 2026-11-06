# Demo capture runbook (silent screen recording)

Reproducible alternative to the OBS + narration master described in
[`video-script.md`](video-script.md). This profile records the same happy
path at 1080p with Playwright, then converts to MP4. No audio, no zoom, no
terminal or multi-window chapters; use it as a draft, a review artifact, or
a base track for a narrated master.

## Prerequisites

- Compose stack up (`make compose-up`) and the web build current:
  `pnpm --filter @eprocurement/web build`.
- Playwright browsers installed (`pnpm --filter @eprocurement/e2e exec playwright install chromium`).
- `ffmpeg` on PATH for the MP4 conversion.

## Record

```bash
cd e2e
DATABASE_URL="<compose DATABASE_URL>" \
E2E_WEB_URL="http://127.0.0.1:3000" \
E2E_GATEWAY_URL="http://127.0.0.1:8080" \
npx playwright test --config=playwright.video.config.ts specs/happy-path.spec.ts
```

The capture profile sets `slowMo` (currently 3800 ms) so each screen has
reading time; adjust it to change the take length. Output lands in
`e2e/test-results-video/<test>/video.webm`.

## Convert

```bash
ffmpeg -y -i video.webm -vf "scale=1920:1080,fps=25" \
  -c:v libx264 -preset medium -crf 20 -pix_fmt yuv420p -an demo.mp4
```

Title and end cards, plus the Grafana and Jaeger clips, are produced as
separate segments and joined with the concat demuxer; keep every segment at
the same resolution, frame rate, pixel format and codec settings so
`-c copy` works.

## Known pitfalls

- **Port 3000 already in use.** A leftover `next start` can hold the port
  (sometimes bound to IPv6 only), which makes Playwright fail with
  `EADDRINUSE`. Stop the stale process before recording.
- **Health check target.** The app root answers 404 by design, so the
  profile checks `/login` instead of `/`.
- **Stale build.** `next start` against an old `.next` serves 404 for every
  route. Rebuild the web app before recording.
- **Bind address.** The profile starts `next start` with `-H 0.0.0.0` so the
  `127.0.0.1` health check succeeds.

## Observability clips

Grafana (`http://127.0.0.1:3001`, dashboard `eproc-overview`) and Jaeger
(`http://127.0.0.1:16686/search?service=gateway`) are recorded by a separate
Playwright script with its own browser context, then converted with the same
settings and concatenated after the application flow.
