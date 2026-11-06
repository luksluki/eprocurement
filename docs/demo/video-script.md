# Demo video: 2:38 cut (EN narration, burned-in captions)

Status: **recorded, not yet uploaded.** The unlisted URL is added to
[`README.md`](../../README.md#demo--review-artifacts) and the dev note once the
upload exists.

| Property | Value |
|---|---|
| Duration | **2:37.9** (157.88 s), measured with `ffprobe` |
| Resolution | 1920×1080, 30 fps, H.264 CRF 20 |
| Audio | AAC mono, TTS narration (English) |
| Captions | Burned in, plus [`demo-captions.en.srt`](demo-captions.en.srt) |
| Size | 5.64 MB |

## Why this replaced the 5:35 plan

The earlier plan in this file budgeted 5:35 across ten chapters, including
three terminal steps, the MinIO console, and a resilience chapter. The cut
that was actually produced covers the application flow end to end, plus the
Grafana and Jaeger chapters, in 2:38.

What was dropped, and why:

- **Terminal `curl` steps.** They need a second capture surface and would add
  roughly 30 s of terminal footage. The same calls are asserted by
  `make e2e` and documented in [`docs/demo-script.md`](../demo-script.md).
- **MinIO console.** The attachment flow is already visible in the UI as a
  presigned upload that reaches `ready`.
- **Resilience chapter.** Covered by `make test-p2p` and the worker-crash e2e
  spec; recording it would require stopping the worker mid-capture.

This is a deliberate scope reduction, recorded here so the issue acceptance
criteria stay honest.

## Production method

1. `e2e/playwright.video.config.ts` records `specs/happy-path.spec.ts` at
   1080p with `slowMo`, against the local Compose stack.
2. Grafana and Jaeger are captured in a second Playwright run with a separate
   context.
3. `ffmpeg` converts each clip to H.264, adds title and chapter cards, and
   concatenates with the concat demuxer.
4. Narration is generated with `edge-tts` (`en-US-AndrewNeural`, `-4%`), one
   file per segment, then delayed and mixed to the chapter starts.
5. Captions are generated from the narration text, snapped to detected pauses
   (`silencedetect`, `-35 dB`, 0.22 s) and burned in with libass.

TTS is used, and that is disclosed here and in the video description. No
maintainer voice recording was made for this cut.

## Timeline

| Start | Segment | Duration |
|---|---|---|
| 00:00 | Title card | 13.0 s |
| 00:13 | Application flow (login, tenant switch, PR, approval, PO, GRN, invoice, 3-way match) | 101.7 s |
| 01:54.7 | Grafana chapter card | 1.5 s |
| 01:56.2 | Grafana dashboard (outbox lag, DLQ depth) | 16.7 s |
| 02:12.9 | Jaeger chapter card | 1.5 s |
| 02:14.4 | Jaeger trace (gateway to procurement to worker) | 16.4 s |
| 02:30.8 | End card and closing narration | 7.0 s |

Chapter timestamps for the video description:

```
00:00 Intro
00:13 Application flow
01:56 Grafana
02:14 Jaeger
02:32 Closing
```

## Narration (as recorded)

| Segment | Line |
|---|---|
| Intro | ELCODE ProcureKit is a multi-tenant procure-to-pay platform. This is the full slice: a requisition, its approval, a purchase order, a goods receipt, and a three-way invoice match. |
| Flow | Everything runs on a local Docker Compose stack: five Go services, Postgres, Mongo, Redis, RabbitMQ, MinIO, and OpenTelemetry. The web app talks only to the gateway. We start as the admin of Acme. The tenant switcher proves isolation: switching to Glow changes the vendor list, because every table is protected by row-level security. Back in Acme, we raise a requisition. The wizard takes four steps: department and purpose, vendor, line items with quantity and price, then a review. The seeded vendor is E2E Acme Supplies, one line item, and the totals are derived, not typed. The requisition is submitted and enters the approval chain. Now the rule that matters: self-approval is forbidden, even for admins. So we sign out and sign back in as the approver in the same tenant. The requester's view shows no action buttons; the approver can act. Approved. Next, the requisition is converted to a purchase order through the gateway API, because the convert screen does not exist yet. That gap is tracked as an issue. The PO moves from draft to issued, then to acknowledged by the vendor. Only then can we record a goods receipt, and the receipt claims exactly the ordered quantity. Finally the invoice, created against the PO, with a PNG attachment uploaded straight to MinIO through a presigned URL. The attachment has to reach ready before the match will run. The three-way match compares requisition, receipt, and invoice. Quantities and prices line up, and the verdict is matched. Every step writes an audit event and publishes to an outbox; the worker consumes it into read models. |
| Grafana | Prometheus scrapes the services' internal metrics. In Grafana, the eProcurement overview shows outbox lag and dead-letter depth. Lag returning to zero means the worker has drained the queue. A rising dead-letter count is what the alerts watch. |
| Jaeger | Jaeger shows the same request as one trace. The gateway span parents the gRPC call into procurement, the database query, the outbox publish, and the consumer on the worker. One purchase order, one trace, end to end. |
| Closing | ProcureKit is built by ELCODE. The code, the runbook, and the demo script are in the repository. |

Timing adjustments: the flow line was trimmed by one clause to fit the 5 s
window of the original plan, and the Grafana and Jaeger lines run at 1.13× and
1.05× `atempo`. The narration now follows the 2:38 cut.

## Captions

`demo-captions.en.srt` has 32 cues, the last ending at 02:37.4. Cue boundaries
were snapped to detected silences within a 1 s radius so no cue cuts a word in
half. The same file is uploaded to YouTube as English captions.

## Upload checklist

1. Upload the MP4 as **unlisted**, 1080p30.
2. Paste the description block: one paragraph plus the chapter list above.
3. Add a line: "Narration is synthetic (TTS). Captions are burned in and also
   available as English subtitles."
4. Upload `demo-captions.en.srt` as English captions and spot-check three cues.
5. Verify the chapters render in the player.
6. Copy the URL into `README.md` and the dev note, then mark the PR ready.

## Known gaps

- No terminal chapter, no MinIO console, no resilience chapter (see above).
- Recorded against the local Compose stack with synthetic demo data.
- TTS narration, not a human voice.
