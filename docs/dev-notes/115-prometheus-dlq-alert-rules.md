# Dev note — #115 Prometheus alerting rules for the DLQ (depth + dead-letter rate)

- **Issue:** #115 (`area:observability`, milestone M5)
- **Branch:** feature/115-prometheus-dlq-alert-rules
- **Date:** 2026-11-05

> **Provenance:** the three substantive decisions this PR rides on were
> recorded by the owner in issue #115 comments, each explicitly self-applied
> with **"maintainer override welcome in PR review"** — none is an explicit
> maintainer sign-off (all comments `luksluki`, association owner, verified
> via `gh issue view 115 --comments`):
>
> 1. **A small Go counter ships with the rules: `dlq_deliveries_total{reason}`**
>    (four call sites, the #55 metrics pattern, no new dependency). The issue
>    title reads rules-only, but its own acceptance criterion demands a
>    dead-letter *rate* alert via `increase()` — the planning pass proved by
>    grep that no rate-able series exists (the depth gauge updates hourly),
>    and the depth gauge itself was Go code added by #48.
> 2. **The merged #48 runbook thresholds win over the draft issue's example**:
>    WARN = depth ≥ 1 for more than 1 hour, CRITICAL = depth ≥ 50
>    (docs/ops/dlq-monitor.md:139-140, written to be formalized into rules);
>    the ">0 for 15m" in the issue text was an illustrative placeholder. The
>    rate alert's 10-minute window is this PR's own documented decision.
> 3. **The one-line compose passthrough `DLQ_SUMMARY_INTERVAL:
>    ${DLQ_SUMMARY_INTERVAL:-1h}`** in the worker env (golden rule 10,
>    `touchesInfra`): the knob already exists in code
>    (services/worker/config/config.go) and in `.env.example`; compose only
>    completes its reachability so a firing demo does not need ~2 h at the
>    default hourly gauge cadence. Default stays 1 h — production unchanged.
>
> A fourth parked process question (the `in-progress` label) was answered as
> protocol, not decision: the label STAYS during active work and comes off
> at issue close.

## What changed

- **`internal/platform/metrics/metrics.go`** — new counter family
  `dlq_deliveries_total{reason}` (`CounterVec`) with exported constants
  `DLQReasonDecodeFailed` / `DLQReasonInvalidEnvelope` /
  `DLQReasonInvalidEvent` / `DLQReasonMaxRetries` (the closed reason set of
  the consumer's dead-letter log lines) and the `IncDLQDeliveries(reason)`
  setter. **`New()` pre-creates all four reason series at 0**
  (`WithLabelValues(reason).Add(0)`) — see Decisions for why this is not
  cosmetic. Every Go process constructs `metrics.New()`, so all five
  processes export the four zero series; the alerts scope to
  `{job="worker"}` (only the worker's runners can dead-letter — the other
  four are structurally 0), and cardinality is bounded by the closed set.
- **`services/worker/consumer/consumer.go`** — `Config.OnDeadLetter
  func(reason string)` (optional, documented: runs inline in the serial
  settle loop, must be cheap), stored on the `Runner`, invoked via the
  nil-guarded `notifyDeadLetter` at all four dead-letter sites
  (`decode_failed`, `invalid_envelope`, `invalid_event`, `max_retries`) —
  each call sits directly under the log line carrying the same reason
  string, so hook and log cannot drift.
- **`services/worker/cmd/worker/main.go`** — both `consumer.New` calls (the
  #47 documents.history/notifications runner and the #53 exports runner;
  they share `handle()`) wire `OnDeadLetter: mc.IncDLQDeliveries`.
- **`deploy/compose/prometheus/alerts.yml`** — new group
  **`eprocurement-dlq`** (already mounted by compose and referenced by
  `rule_files` since #55):
  - `DLQDepthWarn` — `dlq_depth_messages{job="worker"} > 0`, `for: 1h`,
    severity warning (the runbook's WARN tier).
  - `DLQDepthCritical` — `dlq_depth_messages{job="worker"} >= 50`, no
    `for`, severity critical (the runbook's CRITICAL tier).
  - `DLQDeadLetterRate` —
    `increase(dlq_deliveries_total{job="worker"}[10m]) > 0`, severity
    warning, no `for` (see Decisions).
  - Header rewritten: the "#55 must not grow a DLQ rule" note is replaced
    by the group list and the `{job="worker"}` rationale.
- **`compose.yaml`** — worker env gains the `DLQ_SUMMARY_INTERVAL`
  passthrough (owner decision 3, default `1h`).
- **`.env.example`** — the worker comment block now says compose passes
  `DLQ_SUMMARY_INTERVAL` through (and suggests `15s` for an alerting demo);
  the `WORKER_*` knobs remain compose-unset.
- **`deploy/compose/grafana/dashboards/eprocurement-overview.json`** — the
  DLQ depth panel description's stale "Alert rules stay with #54" now names
  `DLQDepthWarn`/`DLQDepthCritical` and the rules file.
- **`docs/ops/dlq-monitor.md`** — §"Alerting thresholds" gains a paragraph
  naming the three alerts + rules file + scope, why the rate alert exists
  (hourly gauge vs minutes-scale counter), and the post-demo purge step.
- **Tests** — `internal/platform/metrics/metrics_test.go`
  (`TestDLQDeliveriesCounter`: exactly four zero series right after `New()`;
  `IncDLQDeliveries` bumps only the named reason);
  `services/worker/consumer/consumer_test.go`
  (`TestHandleDeadLetterHook`: table over all four reasons, hook fires
  exactly once with the right reason, nil hook safe — no broker needed:
  `Nack` on a delivery without an `Acknowledger` is a nil-guarded no-op in
  amqp091-go); `services/worker/consumer/consumer_integration_test.go`
  (`TestDeadLetterHookReasons`: one poison per reason through the REAL
  broker settle loop + a healthy control delivery that must not fire the
  hook; `startRunner` gained optional Config mutators).

## Decisions

- **Pre-initialization of the four reason series at 0 (required, not
  cosmetic).** `/metrics` is served with plain `promhttp.HandlerOpts{}`
  (internal/platform/metrics/http.go:32). Verified in the pinned
  client_golang v1.23.0 source (module cache,
  `prometheus/promhttp/http.go:194,218`): both `EnableOpenMetrics` and
  `EnableOpenMetricsTextCreatedSamples` are opt-in bools, default false —
  the exposition carries no created-timestamp/`_created` information. A
  counter series that Prometheus first sees mid-window already at its final
  value therefore yields `increase() = last − first = 0`: with lazy series
  creation, one dead-letter per reason would NEVER fire
  `DLQDeadLetterRate` (the demo's own pattern — exactly one poison per
  reason, like `TestDeadLetterHookReasons`). With the series at 0 since
  process start, a single dead-letter is a 0→1 step between two scrapes
  (15 s scrape interval) and fires deterministically. This also closes a
  production blind spot: a worker that dead-letters before its first
  scrape. Side effect (accepted): all five processes export four zero
  series; the alerts scope `{job="worker"}`.
  Rejected alternatives, recorded for the maintainer:
  - *(b) demo-only: publish a second poison ≥1 scrape interval after the
    first* — fixes the demo, not production, and makes the alert's meaning
    depend on accident of timing;
  - *(c) enable OpenMetrics created timestamps in promhttp* — changes the
    `/metrics` content negotiation and exposition format shared by all
    five services (identity/le/quantile caveats, `_created` cardinality
    warnings in client_golang's own doc comments). More invasive than four
    zero series; left as a follow-up for the maintainer.
- **`DLQDeadLetterRate` severity = warning, no `for`.** The issue text did
  not pin a severity for the rate alert. One dead-letter in ten minutes is
  exactly the runbook's WARN philosophy ("a single poisoned message is
  worth a look"); depth escalation is already covered by the depth alerts.
  No `for`: the window itself is the debounce, and the counter only moves
  on real dead-letters. This PR's own decision — override welcome.
- **Depth alerts ride the gauge as-is** (hourly by default): the runbook's
  thresholds were written against the summary cadence ("one hour matches
  the summary cadence so it does not page at night"), so `DLQDepthWarn`'s
  `for: 1h` and no window gymnastics. Operators who want faster depth
  reaction lower `DLQ_SUMMARY_INTERVAL` (now reachable via compose) or read
  the rate alert, which is the minutes-scale signal.
- **Counter label set = the log reasons, kept in sync by construction** —
  the hook call sits under the log line with the same literal; the metrics
  test pins the exact four series and the consumer tests pin the exact
  four strings.

## Evidence

All commands run from the repo root on 2026-11-05 (Windows/Git Bash, Docker
Desktop; local compose stack). gofmt note: `gofmt -l` lists the touched Go
files because the working copies are CRLF (`core.autocrlf=true`, index LF) —
the pre-existing repo-wide state noted in dev-notes 045/046/048, identical
on develop (`git ls-files --eol` → `i/lf w/crlf`).

- `go vet ./... && go build ./...` — clean (run together with the full
  suite below).
- `go test ./internal/platform/metrics/ ./services/worker/consumer/
  -count=1` — ok (unit; integration suites skip without TEST_* vars).
- `TEST_RABBITMQ_URL=amqp://eprocurement:eprocurement@localhost:5672/
  TEST_MONGO_URI=mongodb://eprocurement:eprocurement@localhost:27017/
  ?authSource=admin go test ./services/worker/consumer/ -count=1` — ok
  twice (once `-run 'TestDeadLetterHookReasons|…'` filtered, once full
  package): the new end-to-end reason test passes against the real broker +
  Mongo (one dead-letter per reason, healthy control does not fire the
  hook), alongside the existing retry/permanent/shutdown suites.
- `docker compose config --quiet` and
  `docker compose --profile observability config --quiet` — both valid.
- **promtool** (the pinned Prometheus image, AC's validation path):
  `docker run --rm --entrypoint promtool -v <repo>/deploy/compose/
  prometheus/alerts.yml:/alerts.yml:ro prom/prometheus:v3.6.0 check rules
  //alerts.yml` → `SUCCESS: 4 rules found` (1 export + 3 DLQ).
- **Live firing demo** (rebuilt `worker` and `gateway` images, then
  `DLQ_SUMMARY_INTERVAL=15s docker compose --profile observability up -d
  --wait` → all healthy; `docker compose ps` lists 14 services incl.
  prometheus/grafana/jaeger):
  - Baseline: `wget -qO- localhost:9104/metrics` inside the worker
    container shows the four `dlq_deliveries_total{reason=…}` series **at
    0 from boot** (pre-initialization live), and Prometheus
    (`localhost:9090/api/v1/query`) returns them for job=worker; all five
    targets `up`.
  - `GET /api/v1/rules` → groups `eprocurement-dlq` (DLQDepthWarn,
    DLQDepthCritical, DLQDeadLetterRate) + `eprocurement-export`
    (ExportAgeStale), all `inactive`, health ok.
  - Published ONE undecodable poison to `procurement.events`
    (routing key `grn.posted`, RabbitMQ management API): worker log
    `{"msg":"delivery dead-lettered", … "reason":"decode_failed"}`; within
    ~20 s `dlq_deliveries_total{job="worker",reason="decode_failed"}` = 1
    and **`DLQDeadLetterRate` state=firing** (value ≈ 1.06 — increase()'s
    extrapolation over the 0→1 step), while **`DLQDepthWarn` state=pending**
    (`for: 1h` — shown pending, per plan, not waited out). Critical absent
    (depth 1 < 50).
  - Published 50 more poisons → `rabbitmqctl list_queues` shows
    `procurement.dlq 51`; after one accelerated gauge tick (15 s interval;
    `docker compose exec worker printenv DLQ_SUMMARY_INTERVAL` → `15s`)
    `dlq_depth_messages{job="worker"}` = 51 and **`DLQDepthCritical`
    state=firing** (severity critical, value 51); rate alert value ≈ 53 in
    the window; worker logs counted 51 `delivery dead-lettered` lines.
  - **Cleanup (mandatory):** `docker exec eprocurement-rabbitmq-1
    rabbitmqctl purge_queue procurement.dlq` (docs/ops/dlq-monitor.md:127)
    → queue 0; after the next tick the gauge returned to 0 and
    `DLQDepthWarn` + `DLQDepthCritical` left the alerts list.
    `DLQDeadLetterRate` kept firing until the 51 dead-letters aged out of
    the 10-minute `increase()` window (cumulative counter — by design;
    verified resolved afterwards). Stack left with an empty DLQ.
- pnpm build/lint/test — **not run by me** (no TypeScript touched); the
  workflow script runs the full gate set after this note, per the ask.
- Incidental observation, not this diff: the 23 h-old gateway container
  (image built 2026-11-03, before this branch) served `/metrics` with HTTP
  500; rebuilt from the current tree it serves 149 metric lines including
  the four zero series. Pre-existing stale-image condition, worth a
  `docker compose build` on the next stack refresh.

## Follow-ups

- **PR body must carry the sign-off items** (verification list): the three
  owner decisions quoted above (Go counter, runbook thresholds over the
  issue draft, compose passthrough) — recorded by the owner with
  "maintainer override welcome in PR review", not explicit maintainer
  sign-off — plus this PR's own decisions: severity warning for
  `DLQDeadLetterRate`, and the pre-initialization (option (a)) with the
  rejected alternatives (b)/(c).
- **OpenMetrics created timestamps in promhttp** (option (c)) remain a
  maintainer call: they would make counter-window semantics exact for every
  future counter at the cost of an exposition-format change shared by all
  five services.
- **Alertmanager** (routing/receivers for severity warning vs critical) is
  out of scope — the rules are alertmanager-ready (`severity` labels) but
  no alertmanager service exists in the compose profile yet.
- The rate alert inevitably keeps firing up to 10 minutes after a burst is
  purged (window semantics); operators wanting silence-on-purge can pair it
  with `dlq_depth_messages == 0` in a routing rule later.
- Grafana panel for `dlq_deliveries_total` by reason is a natural companion
  to the depth panel — deliberately not added (issue scope: alert rules).
