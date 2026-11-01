# 055 — Prometheus metrics and Grafana dashboard

- **Issue:** #55
- **Branch:** feature/55-prometheus-metrics-grafana-dashboard
- **Date:** 2026-11-01

> **Provenance:** the two open questions on issue #55 were answered by the
> owner's issue comments, but neither is an independent maintainer approval
> — comment 1 (2026-10-31) parks the questions, comment 2 (2026-10-31)
> states "Decisions applied by the agent … maintainer override welcome in
> PR review" (both `luksluki`, verified via `gh api …/issues/55/comments`).
> The applied decisions, both **flagged for maintainer sign-off in the PR
> body** (golden rule 10):
>
> 1. **`github.com/prometheus/client_golang` v1.23.0** is added to go.mod —
>    the issue title mandates Prometheus metrics and `cat go.mod` (before
>    this PR) proves the dependency was absent; the #49 go-redis precedent
>    (new dependency disclosed in the PR body) is the model. v1.23.0 is
>    pinned, not latest (v1.25.0): the newer releases raise their `go`
>    directive to 1.25/1.26 while the repo pins `go 1.24.0` and
>    `deploy/docker/service.Dockerfile` builds on `golang:1.24-alpine`
>    (caught live: the first image build with v1.25.0 failed inside the
>    container). The only other go.mod movement is the MVS floor
>    `github.com/klauspost/compress v1.17.11 → v1.18.0` (client_golang's
>    own requirement; go 1.22 directive — x/* stay untouched).
> 2. **Scope follows the issue titles, not the stale deferral comments**:
>    #55 = metrics + Prometheus scrape + Grafana under the observability
>    compose profile, **without** the OTel SDK — traces/Jaeger stay #54's
>    (still OPEN). The outdated "#54" deferral pointers are corrected by
>    this PR (list below).

## What

- **`internal/platform/metrics` (new)** — the per-process Prometheus
  surface every Go binary now serves on `METRICS_PORT`:
  - `metrics.go` — `Collectors`: a PRIVATE `prometheus.Registry` (plus the
    standard Go/process collectors) carrying
    `http_request_duration_seconds{method,route,status}` (gateway; the
    route label is the route **template**, e.g. `/api/v1/vendors/:id`, so
    cardinality stays bounded),
    `grpc_request_duration_seconds{method,code}`,
    `outbox_lag_seconds`, `dlq_depth_messages`, and
    `export_age_seconds{tenant}`. Latency buckets resolve the PRD §19
    targets (sub-100 ms granularity up to 10 s).
  - `grpc.go` — `GRPCUnaryInterceptor()` measuring every unary call;
    `grpcserver.WithMetrics` chains it right after the trace interceptor,
    so the duration covers RBAC + handler and the code label is the final
    status (Internal after a recovered panic).
  - `http.go` — `Serve(ctx, addr, log)`: the `/metrics` HTTP server;
    graceful nil on ctx cancel, a typed error on listen failure so a port
    conflict fails the process instead of running unobserved.
    `ServeOn` variant for tests (`:0` listeners).
  - `metrics_test.go` — exposition/lifecycle (endpoints served, graceful
    stop, listen-failure), histogram label fidelity (HTTP incl. a 503
    failure row; gRPC OK + NotFound), gauge setters incl. the
    delete-then-recreate export-age semantics.
- **Services** — all five processes construct `metrics.New()`, log
  `metrics_port` at boot, and run `Serve` beside the main listener:
  - `gateway` (`9100`): `server.WithMetrics` adds the `requestMetrics`
    fiber middleware after the trace middleware. Three fiber subtleties
    are pinned in code comments and tests: route templates only resolve
    after `Next()`; fiber renders errors (the default 404, handler error
    returns) in the ErrorHandler AFTER middleware unwinds — so the status
    is derived from the returned `*fiber.Error`; and fiber's no-route 404
    is a NEWLY allocated error (not the `ErrNotFound` sentinel) with
    `c.Route()` pointing at the last Use route, so 404/405 are labeled
    `route="unmatched"` instead of leaking raw URIs.
  - `identity` (`9101`) / `procurement` (`9102`) / `documents` (`9103`):
    config gains `MetricsPort`; the gRPC interceptor rides the platform
    chain (identity via a new `server.WithMetrics` passthrough option;
    procurement/documents via their existing `grpcserver.Option`
    variadic). On shutdown the main returns the first of {gRPC error,
    metrics error}.
  - `worker` (`9104`): config gains `MetricsPort` +
    `ExportAgeInterval` (`EXPORT_AGE_INTERVAL`, default 60 s). Two new
    components join `runComponents`: `export-age` (below) and `metrics`
    (the `/metrics` server — a listen failure is fatal, like every other
    component error). The publisher loop gets `SetLagSink` (lag = age of
    the oldest pending row, oldest-first, 0 when drained, unchanged on a
    failed poll — never a fake 0); the DLQ monitor gets `SetDepth`
    (every successful tick, unchanged on failure — same principle).
- **`services/worker/export/age_monitor.go` (new)** — the
  `export_age_seconds{tenant}` refresher (the FR-10 "gauge" half of the
  pair dev-note 049 deferred here): enumerate tenants via the register's
  `SELECT DISTINCT tenant_id`, read `job:export:last_success:{tenant}`,
  set the age on a valid heartbeat. **Absent, malformed, or unreadable
  heartbeat → the tenant's series is DELETED** (`GaugeVec
  .DeleteLabelValues`) — an honest data gap, never a faked 0: a missing
  signal can neither fire the 26 h alert for a healthy tenant nor mask a
  stale one behind a fake fresh reading. Tenants that leave the
  enumeration lose their series too (no ghost staleness); an enumeration
  failure keeps the previous values (stale-but-real beats a wipe). The
  `AgeSink` interface keeps the package Prometheus-free (fakes in tests).
- **Compose (`compose.yaml`)** — `prometheus` (pinned
  `prom/prometheus:v3.6.0`) and `grafana` (pinned `grafana/grafana:12.1.0`)
  under `profiles: [observability]`, publishing ONLY
  `${PROMETHEUS_PORT:-9090}` and `${GRAFANA_PORT:-3001}` (3001 so Grafana
  never clashes with `next dev` on 3000). The five services get
  container-internal `METRICS_PORT` env values (9100–9104) — deliberately
  NOT published: only Prometheus reaches them, on the backend network.
  Volumes `prometheusdata`/`grafanadata`; grafana `depends_on` prometheus
  healthy. The two stale comments are corrected: the `#54` observability
  placeholder (line 100) now names #55/#54 split, and the worker's
  "No ports — the worker only dials out" now states the one internal
  metrics port.
- **`deploy/compose/prometheus/`** — `prometheus.yml` (scrape the five
  `job`s at 15 s; `rule_files: alerts.yml`) and `alerts.yml` with exactly
  one rule, **`ExportAgeStale`** (`export_age_seconds > 93600`, i.e.
  26 h = 24 h slot + 2 h grace, FR-10 / dev-note 049:285-287). **DLQ
  alerting is deliberately absent** — dev-note 048:225-226 defers it to
  #54; the file says so at the top so it does not grow a DLQ rule.
- **`deploy/compose/grafana/`** — provisioned datasource (pinned uid
  `eproc-prometheus` → `http://prometheus:9090`), file-provider dashboard
  provisioning, and the committed dashboard
  `dashboards/eprocurement-overview.json`: gateway HTTP latency
  p50/p95/p99, internal gRPC latency p50/p95/p99 per service, outbox lag,
  DLQ depth (thresholds 1/50 per `docs/ops/dlq-monitor.md`), and export
  age by tenant with the 93600 s dashed threshold line (both a
  `vector(93600)` series and panel thresholds).
- **`.env.example`** — `PROMETHEUS_PORT`, `GRAFANA_PORT`,
  `GRAFANA_ADMIN_USER/PASSWORD`, the METRICS_PORT-not-published note, and
  `EXPORT_AGE_INTERVAL`. **README** — the local endpoints table gains the
  Prometheus/Grafana rows + the profile invocation.
- **Stale-pointer corrections (the "Prometheus is #54" comments)** —
  `compose.yaml:100`, the worker comment, `dlqmonitor.go`'s package doc,
  dev-notes 045 (worker "nothing listens", the observability-deferral
  decision, the #54 follow-up), 047 (DLQ-metric deferral decision + #54
  follow-up), 048 (provenance note, interim-metric decision, follow-ups).
  **Two corrections beyond the maintainer-documented list** (the issue
  comment names compose.yaml + 045/047/048), applied for the same
  staleness class and **flagged for maintainer confirmation in the PR
  body** (golden rule 2): dev-note **046:151** ("the DLQ depth metric is
  #54") and the agent-discovered pointers in **`docs/schema/procurement.md`**
  (×4: the #45 observability section, the DLQ-monitor section, backlog rows
  #45/#48) and **`docs/ops/dlq-monitor.md`** (×1). All corrections keep the
  historical voice and mark the change "corrected by #55".

## Decisions

- **Compose profile `observability` is OPT-IN — a documented deviation
  from PRD §14.9.** The PRD (prd-v0.2.md:308) and ADR-0001:28 model
  `prometheus`/`jaeger` as DEFAULT compose services that a `demo` profile
  TRIMS. This PR inverts the direction: the default stack stays lean and
  observability opts in (`docker compose --profile observability up`).
  Basis: the compose placeholder comment ("Observability profile … arrives
  with #54"), the issue's own "Demo profile may skip Grafana", and
  ADR-0003's memory-capped demo stance — running Prometheus+Grafana on
  every local boot serves neither. The deviation is flagged in the PR body
  so it is not read as a §14.9 misparse; Jaeger's eventual service (#54)
  will join the same profile.
- **`export_age_seconds` semantics: gap, not zero** (see age_monitor.go):
  unknown age must not look like a fresh export. This mirrors #49's
  detection logic (absent/malformed heartbeat → fall back, never assume)
  and makes the `ExportAgeStale` rule honest — it can only fire on a
  MEASURED age. Consequence: an operator watching for "export stopped"
  should ALSO alert on `absent(export_age_seconds)` if they want
  heartbeat-total-loss coverage; that alert deliberately does not ship
  here (it would fire on every fresh volume).
- **Failed reads keep the last gauge value** (outbox poll error, DLQ depth
  error, tenant-enumeration error): a stale-but-real number is more
  truthful than either 0 (fakes freshness, can mask an incident) or a gap
  (discards known history). Only the export-age refresher deletes on
  unreadable input, because its alert threshold (26 h) would otherwise be
  trivially masked by a frozen value.
- **One interceptor, platform-owned** (`grpcserver.WithMetrics`): the
  chain order trace → metrics → RBAC → logging → recovery keeps latency
  measurement outside service code entirely; services only construct and
  pass the collectors. The nil-Collectors guard keeps grpcserver's own
  tests metrics-free.
- **Fiber status/route derivation lives in the gateway middleware** (not
  the platform package): fiber stays out of `internal/platform`, and the
  three routing quirks are documented where a future maintainer will trip
  over them.
- **Grafana default 3001** (`GRAFANA_PORT`): 3000 is the web dev server's;
  a clash would break `make dev`-style workflows. Admin password via
  `GRAFANA_ADMIN_PASSWORD` (local default `eprocurement`, local-only like
  every other credential in `.env.example`).
- **Prometheus image tags pinned exactly** (`v3.6.0`, `12.1.0`) per the
  repo convention (cf. `otel …:0.111.0`); both verified to exist on Docker
  Hub at implementation time.

## Evidence

All commands run from the repo root against the local stack (compose up
36+ h; Windows/Git Bash):

- `go vet ./... && go build ./...` — clean.
- Targeted suites, all green:
  `go test ./internal/platform/metrics/ ./internal/platform/grpcserver/
  ./services/gateway/server/ ./services/worker/publisher/
  ./services/worker/dlqmonitor/ ./services/worker/export/ -count=1`
  — new: metrics exposition/interceptor/gauges (5 tests), the
  chain-wiring test (health-check call observed in
  `grpc_request_duration_seconds`), the gateway route-template test
  (`/healthz` 200, `/api/v1/me` 401 ×2, `unmatched` 404), the lag-gauge
  table (oldest age / drained 0 / failed poll silent), the depth-gauge
  table (success publishes, failure silent), and the age-refresher suite
  (heartbeat table, departed tenants, enumeration failure keeps values,
  Run lifecycle, 60 s default).
- `go test ./... -count=1` (whole tree, no TEST_* vars — integration
  suites skip per the repo convention) — 35 packages ok, zero failures.
- `docker compose config --quiet` (default) and
  `docker compose --profile observability config --quiet` — both valid.
  Resolved port audit (JSON config): worker publishes NOTHING, the five
  app services publish only their API ports, and the profile adds exactly
  `9090` (prometheus) + `3001` (grafana) — 9100–9104 are nowhere on the
  host. Default `--services` list is unchanged by the profile services.
- **Live run against the local stack** (rebuilt images, then
  `docker compose --profile observability up -d --wait` → all healthy):
  - `curl localhost:9090/api/v1/targets` → all five jobs
    `documents/gateway/identity/procurement/worker -> up` (scrape URLs
    `http://<svc>:910X/metrics`, no lastError).
  - `curl localhost:9090/api/v1/query?query=up` → five series, all `1`.
  - `curl localhost:9090/api/v1/rules` → group `eprocurement-export`,
    rule `ExportAgeStale` = `export_age_seconds > 93600` (loaded).
  - `curl 'localhost:9090/api/v1/query?query=grpc_request_duration_seconds_count'`
    → real series, e.g. `documents /grpc.health.v1.Health/Check OK`,
    `identity /identity.v1.Identity/GetJwks OK`; after driving gateway
    traffic (`/healthz`, `/api/v1/me`, a 404),
    `sum by (route, status) (http_request_duration_seconds_count)` →
    `/healthz 200`, `/api/v1/me 401`, `unmatched 404` — route templates,
    not raw paths.
  - Worker gauges via Prometheus: `outbox_lag_seconds` 0 (drained),
    `dlq_depth_messages` 367 (the dev broker's real DLQ, matching
    dev-note 048's count), `export_age_seconds{tenant}` present for both
    demo tenants with real ages (~347 000 s ≈ 4 days since the 2026-10-27
    export run) — the refresher reads genuine heartbeats.
  - In-network check (ports are host-invisible):
    `docker compose exec <svc> wget -qO- localhost:910X/metrics` → 41
    metric families on EVERY one of the five services;
    `curl localhost:9100/metrics` from the host → connection refused.
  - Grafana (admin/`GRAFANA_ADMIN_PASSWORD` on `localhost:3001`):
    `/api/datasources` → Prometheus `http://prometheus:9090`
    (uid `eproc-prometheus`); `/api/search` → folder "eProcurement" +
    dashboard "eProcurement overview"; `/api/dashboards/uid/eproc-overview`
    → all five panels (gateway p50/p95/p99, gRPC p50/p95/p99, outbox lag,
    DLQ depth, export age with the 26 h threshold target) — provisioning
    and import succeeded with no manual steps.
- pnpm build/lint/test — **not run by me** (no TypeScript touched); the
  workflow script runs the full gate set after this note, per the ask.

## Follow-ups

- **PR body must carry the sign-off items** (verification list):
  (a) the `client_golang` dependency (+ its transitive go.mod bumps);
  (b) the scope split from #54 (no OTel SDK here);
  (c) the opt-in observability profile deviation from PRD §14.9;
  (d) the stale-pointer corrections beyond the documented list — dev-note
  046:151 + docs/schema/procurement.md (×4) + docs/ops/dlq-monitor.md (×1).
- **#54**: OTel SDK + Jaeger service (same `observability` profile), the
  DLQ alerting rules (dev-note 048), spans + AMQP trace-context
  propagation.
- An `absent(export_age_seconds)` companion alert (heartbeat-total-loss
  coverage) is deliberately not shipped — fresh volumes would page; needs
  a maintainer call on the intended behavior.
- Error-rate and cache-hit-ratio panels (PRD §18 names them) need new
  metrics (#55's issue text lists latency/outbox/DLQ/export-age only) —
  candidates for a follow-up issue or #54's pass.
