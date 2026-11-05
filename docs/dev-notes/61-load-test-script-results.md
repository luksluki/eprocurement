# Dev note — #61 Load test script and results

- **Issue:** #61 (epic #11, milestone M5, `area:testing`, P2)
- **Branch:** feature/61-load-test-script-results
- **Date:** 2026-11-04

## What changed

- **`scripts/loadtest/loadtest.js` (new)** — the k6 scenario, outside the
  pnpm workspace (`scripts/` is not a workspace root — putting it in a
  package would drag it into `tsc --noEmit`). 10 VUs × 2 m default
  (`VUS`/`DURATION` env, both overridable), closed loop, one login in
  `setup()` shared by every VU, weighted mix: ~80% reads (vendors,
  requisitions, purchase-orders, dashboard summary, events page 1, split
  evenly) / ~20% writes (`POST /requisitions` draft +
  `POST /requisitions/:id/submit` — the submit's `pr.submitted` outbox row
  loads the async pipeline). The only threshold is `http_req_failed <
  0.01`. Fail-fast in init when `DURATION` exceeds 14 m: the shared token
  lives 15 m (`AccessTTL`) and the design has no refresh. Per-endpoint
  numbers come from explicit `Trend`/`Counter` metrics
  (`latency_*`/`calls_*`/`failures_*`): current k6 no longer emits
  `http_req_duration{group:::…}` sub-metrics in the summary data — the
  first draft used `group()` and printed only an OVERALL row;
  `handleSummary` renders the per-endpoint table and exports the raw
  metrics to `scripts/loadtest/summary-latest.json` (gitignored).
- **`scripts/dev/loadtest.sh` (new)** — the `make loadtest` wrapper: k6
  presence check (the Makefile `protoc` pattern, with install hints),
  `/healthz` wait, drain → wipe → install → drain → k6 → report. Wipe and
  fixtures follow `demo-story.sh` per-tenant semantics on a dedicated
  load tenant `77777777-…` (grepped first: no exact-form collision; the
  `77777777-0052-*` / `77777777-0000-*` strings live only in store tests
  as scratch tenants). Fixtures: one admin user (`cmd/useradmin`), one
  vendor (409-tolerant), the single-step IDR approval chain. A background
  sampler records queue depth + pending outbox rows every 5 s and
  `docker stats` every 15 s during the run; after k6 it waits for the
  backlog to drain (that wait IS the bottleneck measurement), then prints
  peak CPU per service, per-table row counts, and the Mongo projection
  counts. Ends with the one-line note that back-to-back re-runs inside a
  minute can trip identity's 5/min-per-email login limiter (two logins
  per run: fixture install + k6 setup; the #59/#60 convention).
- **`Makefile`** — `loadtest` target + help line + `.PHONY` (the
  `perf-budget`/`demo` pattern).
- **`docs/perf.md`** — new §9 "HTTP load test (#61)": scenario, re-run
  protocol, the duration-vs-TTL ceiling, the two-run results table,
  hardware, row counts, and the measured bottlenecks with their scale
  caveat; §8's "HTTP-level budgets" follow-up now cross-references §9.
- **`README.md`** — one row in the make-target table.
- **`.gitignore`** — `scripts/loadtest/summary-latest.json` (regenerated
  per run).
- **Not changed:** `compose.yaml`, CI workflows, any Go service, web app,
  `go.mod`/`package.json` (k6 is an external tool, not a repo dependency;
  load-test results are manual evidence, not a CI job — golden rule 10).

## Decisions

- **k6 over autocannon** (the issue text delegates the choice): the
  scenario is a weighted multi-endpoint mix with a shared bearer token in
  `setup()` — k6's native shape; autocannon would need custom Node glue
  for auth + weighted routing.
- **Login exactly once per run, token shared by all VUs.** Identity
  allows 5 logins/min per email+address (`config.go`), the gateway 30/min
  per IP — per-VU logins would measure the limiters. The price is the
  15-minute token TTL with no refresh in the design, hence the hard
  14 m ceiling (fail-fast with an explicit message, exit 107 before any
  request) and the documented safe default (10 VU × 2 m).
- **Drain BEFORE wipe must cover the outbox, not just the queues.** The
  ported `demo-story.sh` queue-drain is necessary (worker-held messages
  re-project into the wiped tenant) but not sufficient at load-test
  scale: a publisher batch SELECTed before the wipe would still publish
  its ~50 rows into the wiped tenant. The pre-wipe gate therefore waits
  for queue depth 0 AND `outbox … published_at IS NULL` count 0. This
  matters because the publisher is slow at this write rate (below) — the
  first calibration run left ~9k pending rows.
- **The drain budget is 600 s, not demo-story's 60 s.** The publisher
  moves 50 rows per 2 s tick (`WORKER_POLL_INTERVAL`/`WORKER_BATCH_SIZE`,
  PRD §16 defaults = 25 rows/s) while the write lane produces ~100
  submits/s — a 2 m run ends with ~9–10k pending rows that legitimately
  take ~6 min to drain. `LOADTEST_DRAIN_BUDGET` is the escape hatch. This
  asymmetry is the run's headline bottleneck, measured twice.
- **Bottleneck claims are scoped to this run's row counts** (starts
  empty, ends ~12k requisitions; `purchase_orders` stays 0 because the
  write chain stops at submit). The dashboard summary's three spend
  queries read an empty `purchase_orders` here; large-data store costs
  stay #56's (perf.md §2–§3). perf.md §9 says exactly this.
- **Latency numbers documented, not gated** — the only k6 threshold is
  `http_req_failed < 1%`; p95 gating/trending stays the §8 follow-up.

## Evidence

All from the repo root against the live compose stack (all services
healthy; k6 v2.3.0 installed via scoop during this issue — the wrapper's
install-hint path was exercised in reverse).

- `bash -n scripts/dev/loadtest.sh` — clean.
- `DURATION=20m k6 run scripts/loadtest/loadtest.js` — fails fast before
  any request: `DURATION "20m" exceeds the 14 minute ceiling …` exit 107.
- **`make loadtest` — run 1: PASS** (exit 0): 71 274 requests at
  593.5 req/s, `http_req_failed` 0.00%, checks 100%, 59 354 iterations
  (write share 20.1%). Per-endpoint (med/p95/p99 ms): vendors 4.7/6.3/7.6;
  requisitions 16.8/27.3/30.9; purchase-orders 3.3/4.6/5.9; dashboard
  summary 54.3/105.4/120.2; events 7.6/12.1/14.3; POST requisition
  16.0/27.3/105.9; POST submit 9.5/13.5/28.2. During-run maxima: queue
  depth 1, pending outbox 8 878. Post-run drain 358 s (~24.8 rows/s).
  Row counts: vendors 1, requisitions/requisition_items/approval_steps/
  outbox all 11 918, purchase_orders 0; Mongo events 11 918,
  notifications 11 918, audit_log 0. Peak CPU: postgres 800%,
  procurement 200%, gateway 122%, mongo 107%, documents 34%, worker 12%,
  rabbitmq 9%.
- **`make loadtest` — run 2 (back-to-back, started right after run 1's
  drain): PASS, numbers comparable** (exit 0): 76 926 requests at
  640.4 req/s, 0.00% failed, 64 235 iterations (write share 19.7%);
  vendors 4.3/5.7/6.8; requisitions 15.7/26.1/29.5; purchase-orders
  3.1/4.2/5.2; dashboard 50.6/100.5/115.9; events 6.9/11.0/12.9; POST
  requisition 14.8/24.9/34.3; POST submit 8.6/11.4/17.3. Maxima: queue
  depth 1, pending 9 509; drain 386 s (~24.6 rows/s); row counts same
  shape at 12 689; CPU postgres 826%, procurement 191%, gateway 127%.
  Run 2's pre-wipe drain passed instantly (run 1's post-run drain had
  already emptied the outbox) — the reproducibility protocol works as
  designed. The twice-measured ~24.7 rows/s drain is the publisher's
  50-per-2s-tick cap; queue depth ≤1 with worker/rabbitmq ≤12% CPU shows
  the consumers are not the constraint.
- `go vet ./... && go build ./...` — clean (no Go code touched, sanity).
- pnpm gates — run by the workflow after this note; nothing in the JS
  workspace changed (scripts/loadtest is outside it).

## Follow-ups

- Publisher throughput under sustained submit load (25 rows/s at PRD §16
  defaults) breaks FR-11's 2 s timeline freshness during the load and
  leaves minutes of projection lag; tuning `WORKER_BATCH_SIZE`/
  `WORKER_POLL_INTERVAL` is a compose-env infra decision (golden rule 10)
  — flagged for the maintainer, not changed here.
- A larger-scale HTTP run (≥100 VUs, longer duration) needs either
  mid-run re-login or a longer-lived token to escape the 14 m ceiling —
  a scenario-design change, out of #61 scope.
- k6 trend storage / CI execution: results are manual evidence by design
  (no CI change); a dedicated perf job is the standing §8 follow-up.
