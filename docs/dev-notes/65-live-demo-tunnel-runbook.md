# Dev note — #65 Live-demo tunnel runbook

- **Issue:** #65 (epic #12, milestone M5, `area:demo`)
- **Branch:** feature/65-live-demo-tunnel-runbook
- **Date:** 2026-11-04

## What changed

- **`docs/ops/live-demo-tunnel.md` (new)** — the runbook implementing
  ADR-0003's "live demos via Cloudflare Tunnel on demand" mitigations,
  with the four scoped sections: (1) prerequisites & setup — `cloudflared`
  install per OS (Windows `winget install Cloudflare.cloudflared`, macOS/
  Linux brew/.deb), the **quick tunnel** (`cloudflared tunnel --url
  http://localhost:3000`, zero account/credentials) as the default path,
  and a **named tunnel** appendix (stable URL, credentials rules);
  (2) security notes — only `:3000` (web BFF) is ever tunneled, a
  never-tunnel table for every other published port (8080, 8081–8083,
  5432, 27017, 6379, 5672/15672, 9000/9001, 9090, 3001, 16686,
  4317/4318/13133 — from compose.yaml/.env.example), quick tunnel = zero
  credentials, named-tunnel cert/JSON stay in `~/.cloudflared/` outside
  the repo, and the explicit "demo logins are public and administrative"
  warning (both demo users hold admin; credentials published in
  docs/demo-script.md) with the demo-window-only rule; (3) start/stop
  order with the pre-flight `docker compose ps` gate and tunnel-first
  stop; (4) troubleshooting — 429 login throttle, presigned-URL
  `localhost:9000` failure for remote viewers (presenter-local vs
  second-tunnel-MinIO options with the root-credential risk, recommend
  presenter-local), ephemeral URL on restart, and 502/530-vs-stack-health.
- **`README.md`** — the "Live-demo tunnel runbook (Cloudflare Tunnel)"
  artifact line now links the runbook instead of saying "issue #65
  (open)"; the status paragraph drops #65 from the still-open M5 list
  (video #64 and kind manifests remain).
- **`docs/demo-script.md`** — one cross-link paragraph in the pre-demo
  section pointing presenters with remote audiences at the runbook.
- No code, Makefile, or compose changes — the runbook uses existing
  targets (`make compose-up`, `make demo`) plus the `cloudflared` CLI.

## Decisions

- **Quick tunnel is the documented default; named tunnel is an appendix.**
  The quick tunnel needs no Cloudflare account and mints zero credentials
  — there is nothing to leak, which is the strongest possible posture for
  a demo path whose logins are public. The named tunnel is documented for
  its one real advantage (stable hostname) with hard "never commit
  cert/JSON/token" rules.
- **Only `:3000` through the tunnel, and the doc says why.** The BFF is
  the single browser-safe edge (httpOnly cookies, gateway dialed
  server-side — `apps/web/src/lib/gateway.ts`); every other published
  port carries default dev credentials or unauthenticated admin surfaces,
  so the runbook carries an explicit never-tunnel table rather than a
  generic "be careful".
- **Troubleshooting options for presigned URLs, with a recommendation.**
  Option (b) (tunnel MinIO `:9000` as a second tunnel) technically routes
  the uploads but publishes the S3 API with the repo-public default root
  credentials; the runbook recommends (a) presenter-local (presenter
  drives upload steps, remote viewers read-only) and marks (b) as
  not-verified-in-pass + last-resort-with-changed-credentials.
- **Tunnel-first stop order.** Stopping the web app before the tunnel
  leaves a live URL answering 502s at the audience; stopping the tunnel
  first ends the exposure of public admin logins at once — consistent
  with the "demo window only" rule.
- **README status paragraph updated alongside the artifact line.** The
  paragraph claimed the tunnel runbook was still open; leaving it stale
  would contradict the new link (same concern, two lines).

## Evidence (the runbook was executed end-to-end on this machine)

Environment: Windows (win32), Docker Desktop server 27.3.1. All commands
run in Git Bash from the repo root on 2026-11-04. The ephemeral URL below
is already dead (tunnel stopped, last check 530) — quick tunnel URLs are
random words and carry no credentials.

- **Install:** `winget install --id Cloudflare.cloudflared …` →
  "Successfully installed"; new shell: `cloudflared --version` →
  `cloudflared version 2026.10.7 (built 2026-10-30T09:43 UTC)`.
- **Pre-flight:** `docker compose ps` → 11 containers Up (healthy)
  (postgres, mongo, redis, rabbitmq, minio, otel-collector, identity,
  procurement, documents, worker, gateway — stack already running from
  the #63 session; counts as the healthy precondition the runbook gates
  on).
- **`make demo`** → full install summary: tenant `55555555-…`, both demo
  users ready (admin), vendor DEMO-VND-01, rules + tolerances installed,
  "next numbers PR-000001 / PO-000001 / GRN-000001 (counters reset)".
- **Web:** `pnpm dev` (background) → `curl http://localhost:3000` →
  `307` (unauthenticated redirect). Port 3000 was free before start.
- **Tunnel up:** `cloudflared tunnel --url http://localhost:3000` →
  banner URL `https://buses-districts-voting-statistical.trycloudflare.com`.
- **Public URL loaded from outside:** initial plain `curl` failed with
  exit 6 — the local ISP resolver answered NXDOMAIN for the fresh
  hostname while `nslookup … 1.1.1.1` already returned the Cloudflare
  edge IPs (104.16.230.132 / .231.132); pinned with
  `curl --resolve …:443:104.16.231.132` → `HTTP 307 via 104.16.231.132`,
  TLS verify 0, `<title>eProcurement</title>`; ~1 minute later the same
  plain curl (no pin) → `HTTP 200 via 2606:4700::6810:e684` on
  `/login`. This became the "fresh URL does not resolve" troubleshooting
  bullet. (A first tunnel attempt via bare `cloudflared` in a background
  shell failed with exit 127 — PATH not yet visible there; rerun with the
  full install path. Install-path visibility is why the runbook says
  "open a new terminal".)
- **Demo login through the public URL:** `POST <tunnel>/api/auth/login`
  with `demo-requester@story.example` → `HTTP 200` via the edge,
  `{"ok":true,"redirect":"/app/dashboard"}`.
- **One PRD §22 story step through the tunnel** (step 1's requisition
  slice, BFF routes, session cookie): `POST /api/requisitions`
  (IDR, empty) → `201`; `PUT /api/requisitions/<id>` (1 line: qty 2 ×
  7 500 000 cents) → `200`; `POST /api/requisitions/<id>/submit` →
  **`"number":"PR-000001"`, `"status":"submitted"`** — the seeded story's
  exact first number, reached from the public edge.
- **Troubleshooting evidence (both documented failures reproduced):**
  - attachment declare on a fresh draft via the tunnel → `201` with a
    presigned PUT whose host is **`http://localhost:9000/…`**
    (`S3_PRESIGN_ENDPOINT` default, compose.yaml) — unreachable for a
    remote viewer, exactly as the runbook's item describes;
  - six rapid logins for the same demo email through the tunnel → five
    `200`, the sixth **`429 "Too many login attempts. Try again in a
    minute."`** (identity's 5/min-per-email limiter).
- **Stop:** `cloudflared` terminated first → `curl <tunnel>/` →
  **`HTTP 530`** from the edge (audience locked out); `pnpm dev` stopped;
  port 3000 free; `ls ~/.cloudflared` → no such directory (the quick
  tunnel created **zero** credential files on this machine).
- **No-credentials sweep:** `git status` — only `README.md`,
  `docs/demo-script.md` modified + `docs/ops/live-demo-tunnel.md` new
  (plus the pre-existing untracked `.zcode/`); `git diff --stat` → 7
  insertions / 2 deletions across the two modified docs. Secret-pattern
  grep (`api[_-]?key`, `bearer …`, `BEGIN (RSA|EC|OPENSSH)`, 20+ char
  token assignments, `X-Amz-Signature`) over the runbook, README,
  demo-script, and this note → no matches. The only credential strings
  that appear are the repo's own published fixtures/defaults
  (`demo-password` seeded story logins; the `eprocurement` local MinIO
  defaults named only as a risk, both already public in docs/demo-script.md,
  compose.yaml, .env.example).
- **Gates (AGENTS.md, full set):** `go vet ./... && go build ./...` —
  clean; `go test ./...` — 40 packages `ok`, zero `FAIL`.
  `pnpm install --frozen-lockfile` — up to date; `pnpm build` — 3/3
  tasks; `pnpm lint` — 4/4; `pnpm test` — 6/6 (turbo-cached: no code
  changed on this branch, so the cache reflects this exact tree).
  Compose/seed gates not run — no compose or seed change in this PR
  (`make demo` above did run clean as part of the end-to-end pass).

## Follow-ups

- `make tunnel` wrapper (compose-up + demo + cloudflared in one target)
  is a possible ops nicety — deliberately not added (docs-only scope per
  the plan; the maintainer can ask for it).
- If remote viewers ever need to drive attachment steps themselves,
  revisit option (b) with rotated MinIO credentials + `S3_USE_SSL=true`
  verification, or a BFF-proxied upload path (presigned URLs would stop
  leaving the web origin).
- Demo video (#64) should reference this runbook for the live-demo
  fallback.
