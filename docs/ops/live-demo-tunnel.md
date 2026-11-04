# Live-demo tunnel runbook (Cloudflare Tunnel) — #65

How to expose the local demo stack to remote viewers on demand, per
[ADR-0003](../adr/0003-demo-hosting.md): the demo always *runs* locally
(`make compose-up` + [`make demo`](../demo-script.md)); the tunnel only
carries the web UI to the audience. There is no always-on public
deployment in Phase 0 — the tunnel exists exactly for the demo window and
is stopped the moment the demo ends (see [Security](#2-security-notes)).

The **quick tunnel** (no Cloudflare account, no credentials) is the
default path. A **named tunnel** (stable URL, your own domain) is the
optional [appendix](#appendix-named-tunnel-optional).

## 1. Prerequisites and setup

### 1.1 Install `cloudflared`

One-time, per machine. Verify with `cloudflared --version` afterwards.

| OS | Command |
|---|---|
| Windows | `winget install Cloudflare.cloudflared` (MSI; adds itself to `PATH` — open a **new** terminal so the PATH change is visible) |
| macOS | `brew install cloudflared` |
| Linux | `.deb`/`.rpm` from the [cloudflared releases page](https://github.com/cloudflare/cloudflared/releases), or `brew install cloudflared` |

The quick tunnel works immediately after install; no `cloudflared login`
and no Cloudflare account are needed.

### 1.2 The local stack (the demo itself)

The tunnel adds nothing to the stack — it only forwards a port. Complete
the [pre-demo](../demo-script.md#0-pre-demo-once) steps first:

```bash
make compose-up        # core stack, waits for healthy
make demo              # reset demo tenant + install the seeded story
# optional, only for demo-script steps 6–7:
docker compose --profile observability up -d --wait
pnpm dev               # web app on http://localhost:3000
```

## 2. Security notes

**Only `localhost:3000` (the web BFF) ever goes through the tunnel.**
That is the one edge built for browsers: the BFF keeps session tokens in
httpOnly cookies and dials the gateway itself (`apps/web/src/lib/gateway.ts`),
so a remote viewer needs nothing but the web origin.

Every other published port stays loopback-only — **never** add them to a
tunnel (`cloudflared tunnel --url` takes exactly one origin, the web app):

| Port(s) | Service | Why it must not be tunneled |
|---|---|---|
| 8080 | gateway REST edge | server-to-server; the BFF is the only intended browser path |
| 8081 / 8082 / 8083 | identity / procurement / documents gRPC | internal RPC, no browser TLS story |
| 5432 / 27017 / 6379 | PostgreSQL / MongoDB / Redis | databases — default dev credentials, no edge auth |
| 5672 / 15672 | RabbitMQ AMQP / management UI | broker + admin UI with weak local logins |
| 9000 / 9001 | MinIO S3 API / console | root-account S3 API + console with default local credentials |
| 9090 / 3001 / 16686 | Prometheus / Grafana / Jaeger | admin/observability UIs, unauthenticated or weak local logins |
| 4317 / 4318 / 13133 | OTel collector | telemetry ingestion, unauthenticated |

Further rules:

- **Quick tunnel = zero credentials.** It creates an ephemeral
  `*.trycloudflare.com` URL with no account, token, or login — nothing to
  leak.
- **Named tunnel credentials stay outside the repo.** The account cert
  (`~/.cloudflared/cert.pem`) and tunnel credentials
  (`~/.cloudflared/<tunnel-id>.json`) live in `~/.cloudflared/` only.
  Never copy them into the repository, never commit them, never paste a
  tunnel token into chat or a screenshot.
- **The demo logins are public and administrative.** Both seeded story
  users hold the **admin** role, and their credentials are published in
  [`docs/demo-script.md`](../demo-script.md) (they are the point of the
  demo). Anyone watching — or anyone who guesses the URL while it lives —
  can sign in with full tenant-admin power. Therefore: the tunnel exists
  **only for the demo window**. Start it right before the audience needs
  it, share the URL over a private channel, and stop it immediately after
  (step 3.2).

## 3. Start and stop order

### 3.1 Start (in this order)

```bash
# 1. Pre-flight: the stack must be healthy BEFORE the tunnel exists.
docker compose ps                    # every service Up (healthy)
# (if not: make compose-up)

# 2. Seeded story installed (PR-000001 will be the first number).
make demo

# 3. Web app up and answering locally.
pnpm dev                             # http://localhost:3000
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:3000   # 307

# 4. Start the quick tunnel (foreground; keep this terminal).
cloudflared tunnel --url http://localhost:3000
```

`cloudflared` prints the public URL in its startup banner, e.g.:

```text
https://<random-words>.trycloudflare.com
```

Share that URL with the audience. A viewer opening it gets the same app
you have on `localhost:3000` — sign-in, the wizard, the story.

### 3.2 Stop (immediately after the demo)

```bash
# 1. Kill the tunnel FIRST — this locks the audience out at once.
#    (Ctrl+C in the cloudflared terminal)
curl -s -o /dev/null -w '%{http_code}\n' https://<your>.trycloudflare.com/   # 530 = closed

# 2. Stop the web app (Ctrl+C in the pnpm dev terminal).

# 3. Optional — tear the stack down between demo days:
make compose-down
```

The stop order matters: stopping `pnpm dev` first would leave a live
tunnel answering 502s while the audience reloads; stopping the tunnel
first ends the exposure cleanly.

## 4. Troubleshooting

- **Stack unhealthy / 502 or 530 at the public URL** — run
  `docker compose ps` *before* starting (or while debugging) the tunnel.
  A quick tunnel faithfully forwards a broken origin: an unhealthy
  container looks exactly like a tunnel problem (530 = cloudflared lost
  its origin, 502 = origin up but erroring). Fix the stack first; the
  tunnel needs no fixing.
- **Login fails with 429 "Too many login attempts."** — identity
  throttles **5 logins/min per email** (`IDENTITY_LOGIN_RATE_PER_MIN`,
  default 5 — see `apps/web/.env.example`). Rehearsals + the live demo on
  the same two demo emails exhaust the budget quickly. Wait a minute, or
  set `IDENTITY_LOGIN_RATE_PER_MIN=60` in `.env` and `make compose-up`
  for rehearsal-heavy days (demo-script's own advice). Note that remote
  viewers sharing the URL all appear as one client IP behind the BFF.
- **Attachment upload/download fails for remote viewers** — presigned
  MinIO URLs embed `S3_PRESIGN_ENDPOINT`, whose default is
  `localhost:9000` (compose.yaml). The presenter's browser resolves that
  fine; a remote viewer's browser resolves it to *their own* machine and
  the PUT/GET fails. Options:
  1. **Presenter-local (recommended):** the presenter drives the
     upload/download steps of the story (demo-script steps 1 and 4) on
     the shared screen; remote viewers follow read-only. Zero
     configuration, nothing extra exposed.
  2. **Tunnel MinIO too:** run a second quick tunnel for `:9000` and
     point `S3_PRESIGN_ENDPOINT` at it. This publishes the S3 API with
     its **default root credentials** (the `eprocurement` local defaults
     in compose.yaml are public in the repo) to anyone on the URL —
     not recommended; if you must, change `MINIO_ROOT_USER/PASSWORD`
     in `.env` first and treat it as a last resort. Not verified in
     this runbook's end-to-end pass.
- **The quick-tunnel URL changed** — quick tunnel URLs are **ephemeral**:
  every restart of `cloudflared` mints a new random hostname and the old
  one dies. Restarting the web app or the stack does *not* change it;
  restarting the tunnel does. Re-share the URL after any tunnel restart,
  or use a [named tunnel](#appendix-named-tunnel-optional) for a stable
  one.
- **Fresh URL does not resolve for a viewer ("Non-existent domain")** —
  brand-new `*.trycloudflare.com` hostnames can take ~a minute to reach
  some resolvers (observed: an ISP resolver answered NXDOMAIN while
  Cloudflare's 1.1.1.1 already resolved it). Have the viewer retry after
  a minute or switch DNS; from the presenter machine,
  `nslookup <host> 1.1.1.1` distinguishes propagation from a dead tunnel.

## Appendix: named tunnel (optional)

A named tunnel gives a **stable** URL on a domain you control (no more
ephemeral hostnames) at the cost of a Cloudflare account and credentials
on disk. Same security rules apply — only the web origin, only for the
demo window.

```bash
cloudflared tunnel login                        # browser flow; writes ~/.cloudflared/cert.pem
cloudflared tunnel create eproc-demo            # writes ~/.cloudflared/<tunnel-id>.json
cloudflared tunnel route dns eproc-demo demo.example.com   # a hostname in YOUR zone
```

Create `~/.cloudflared/config.yml`:

```yaml
tunnel: eproc-demo
credentials-file: /home/you/.cloudflared/<tunnel-id>.json
ingress:
  - hostname: demo.example.com
    service: http://localhost:3000
  - service: http_status:404
```

Run it with `cloudflared tunnel run eproc-demo`. The cert
(`cert.pem`), the credentials JSON, and any tunnel token are secrets:
they stay in `~/.cloudflared/`, never in the repository, never in logs
or screenshots. A quick `git status` before committing should never show
`~/.cloudflared` or a `config.yml` you meant to keep personal.
