# 054 — OpenTelemetry instrumentation end-to-end

- **Issue:** #54
- **Branch:** feature/54-opentelemetry-instrumentation-e2e
- **Date:** 2026-11-01

> **Provenance:** the two parked questions on issue #54 were answered by the
> maintainer in the issue comments before implementation (comment 2,
> `luksluki`): **(1)** DLQ alerting rules are OUT of scope (the issue text —
> traces + Jaeger — wins over the stale deferral pointers in dev-notes
> 048/055; a separate issue carries the alerting rules); **(2)** tracing
> defaults approved — sampling `ParentBased(AlwaysSample)` with NO env knob,
> and Mongo spans DEFERRED (CommandMonitor events carry no context, so
> parent linkage would be limited — the same deferral pattern as dev-notes
> 045/047). Comment 1 asked for the `in-progress` label to be removed once
> the questions were answered; it was removed before implementation
> (`gh issue edit 54 --remove-label in-progress`; remaining labels:
> type: feature, area: observability, priority: P1, size: M).

## What

- **`internal/platform/telemetry` (new)** — the shared OTel surface, built
  ONLY on the core modules (no contrib instrumentation; every integration
  point is hand-rolled around APIs the repo already uses):
  - `telemetry.go` — `Setup` builds the process `TracerProvider`
    (`ParentBased(AlwaysSample)`, resource `service.name` +
    `deployment.environment`) with an OTLP **gRPC** exporter and installs
    it plus the **W3C tracecontext + baggage** propagator as otel globals.
    An empty `OTEL_EXPORTER_OTLP_ENDPOINT` keeps the SDK ACTIVE (real
    trace ids for log/outbox linkage) but attaches no exporter, so local
    runs and CI stay silent. `NormalizeEndpoint` accepts the env-convention
    `http://` scheme and strips it for `otlptracegrpc.WithEndpoint`.
    `Shutdown` is the deferred companion.
  - `traceid.go` — `ContextWithTraceIDString(ctx, hex32)`: synthesizes a
    REMOTE parent `SpanContext` from a plain trace id — exactly the #54
    publisher linkage (see Decisions). Never overrides an existing valid
    span; invalid input returns ctx unchanged.
  - `grpc.go` — hand-rolled unary client/server interceptors: the client
    side injects `traceparent` into the outgoing metadata (next to the
    claim headers, on a metadata COPY); the server side extracts it, falls
    back to the legacy `x-trace-id` as a synthesized parent, and starts the
    server span. `grpc.health.v1` / `grpc.reflection` methods are span-free
    so compose healthchecks do not flood Jaeger. `mdCarrier` adapts gRPC
    metadata to `propagation.TextMapCarrier`.
  - `http.go` — the gateway edge middleware `HTTPTrace`: extracts the
    inbound traceparent (legacy `X-Trace-Id` hex32 anchors the same trace),
    starts the server span, **`c.SetUserContext(ctx)`** so
    `forwardContext` (built from `c.UserContext()`) carries the span into
    the gRPC client interceptor, and DERIVES the response `X-Trace-Id`
    header and the `trace_id` local from the span — the legacy REST value
    can never disagree with Jaeger. `/healthz` is span-free.
  - `pgx.go` — `PGXTracer` implements `pgx.QueryTracer`: one span per
    statement named by the SQL verb, with `db.statement` TEXT ONLY — bound
    parameters are never recorded (no secrets/PII in traces).
    `PGXPoolConfig` is the one-line pool wiring.
  - `redis.go` — `RedisHook` (go-redis `Hook`): one span per command and
    per pipeline; arguments never recorded; `redis.Nil` stays an OK span.
  - `amqp.go` — `StartPublishSpan` / `StartConsumeSpan` (producer/consumer
    spans) + `InjectAMQP` / `ExtractAMQP` over an `amqp.Table` carrier;
    `TraceparentHeader = "traceparent"` is the wire header (PRD §18
    "RabbitMQ headers").
  - Tests lock every contract: the synthesized parent keeps the envelope
    trace id with a NONZERO deterministic span id (`traceid_test.go`),
    client/server interceptor round-trip and traceparent-wins-over-legacy
    (`grpc_test.go`), the fiber middleware derivation chain
    (`http_test.go`), pgx/redis span attachment, AMQP inject/extract.
- **`internal/platform/logger`** — `TraceID(ctx)` is now SPAN-FIRST: the
  span context wins over the legacy context value, so log `trace_id` is
  byte-identical to the Jaeger trace id (issue AC 2). The legacy value
  remains the fallback for contexts without spans (cron ticks, boot). Test:
  `TestTraceIDSpanFirst`.
- **`internal/platform/grpcserver`** — the OTel server interceptor is
  chained FIRST, ahead of the legacy `TraceInterceptor`
  (server.go:100-106), so the span context exists before anything reads
  it. Test: `TestOTelInterceptorFirstSpanWinsOverLegacyTraceID` runs a REAL
  server, sends `traceparent` + a DIFFERENT `x-trace-id`, and asserts the
  handler's `logger.TraceID(ctx)` equals the SPAN's id.
- **Gateway** — `server.New` mounts `telemetry.HTTPTrace()` (replacing the
  legacy `traceID()` middleware; `traceIDFrom` unchanged for callers);
  `cmd/gateway/main.go` runs `telemetry.Setup` + deferred `Shutdown` and
  dials identity/procurement/documents with
  `grpc.WithUnaryInterceptor(telemetry.GRPCUnaryClientInterceptor())`.
  Test `TestForwardContextCarriesRequestSpan` pins the whole edge→gRPC
  chain in-process: inbound traceparent → `c.SetUserContext` →
  `forwardContext` → outgoing `x-trace-id` = span id → client interceptor
  `traceparent` = same id → response `X-Trace-Id` = same id. The
  pre-existing `TestApprovalDecideTracePropagation` was updated to the #54
  contract: a non-hex legacy `X-Trace-Id` is REPLACED by the span's
  generated id (derived, never independent); a 32-hex legacy id still
  anchors the same trace (bridge for pre-#54 callers).
- **All five services** — `Setup` + deferred `Shutdown` in every `main`
  (`gateway`, `identity`, `procurement`, `documents`, `worker`); each
  `config.Load` reads `OTEL_EXPORTER_OTLP_ENDPOINT` (no sampling knob —
  maintainer decision). Runtime pools get the pgx tracer via
  `telemetry.PGXPoolConfig`: procurement, identity, AND worker (the worker
  owns its pool for outbox Pending/MarkPublished + the export register —
  PRD §18's "spans around DB" without qualification). The worker's Redis
  client carries `telemetry.RedisHook` (export lock + heartbeat).
- **Worker publisher** (`services/worker/publisher/publisher.go`) —
  `Publish` now: synthesizes the remote parent from `env.TraceID`
  (`publishContext`), starts the `outbox publish <type>` producer span,
  injects `traceparent` into `amqp.Publishing.Headers`, and marks the span
  on broker failure. Tests lock the header format
  `00-<env.TraceID>-<16 hex>-<flags>` and the existing-span/non-invention
  branches.
- **Worker consumer** (`services/worker/consumer/consumer.go`) —
  `traceContext` restores the delivery's trace (traceparent header, else
  the envelope `trace_id` synthesized — the pre-#54 message fallback) and
  each delivery runs under an `amqp consume <queue>` consumer span; the
  retry ladder re-injects the CURRENT span context, so a retried copy
  keeps the same trace. Tests lock header-win / envelope-fallback /
  garbage-header cases.
- **Compose** — `jaeger` service (`jaegertracing/all-in-one:1.62.0`,
  profile `observability`, UI `${JAEGER_UI_PORT:-16686}:16686`;
  `COLLECTOR_OTLP_ENABLED: "true"` pins the default-on OTLP receivers).
  `deploy/compose/otel-collector.yaml`: traces pipeline exporters
  `[otlp/jaeger (endpoint jaeger:4317, tls insecure), debug]` — debug kept
  as the always-on troubleshooting sink; metrics/logs pipelines untouched.
  All five services get `OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector:4317`
  in compose. `.env.example` documents `JAEGER_UI_PORT` and the endpoint
  semantics; README adds the Jaeger UI row.
- **Procurement stores untouched**: the outbox writers already record
  `logger.TraceID(ctx)` (purchaseorder/store.go:187,293; approval:426;
  requisition:321; goodsreceipt:191; match:187), which is now span-first —
  so outbox rows automatically carry the OTel trace id without a schema or
  store change.

## Decisions

1. **Dependency disclosure (golden rule 10, #49/#55 precedent):** this PR
   adds OpenTelemetry to `go.mod` as DIRECT requires, all pinned
   **v1.39.0**: `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`,
   `go.opentelemetry.io/otel/trace`, and
   `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`.
   The plan's "exactly three direct modules" was NOT achievable as stated:
   `otel/trace` is a separate Go module carrying the span API
   (`trace.SpanContext`, `trace.Span`) that any hand-rolled instrumentation
   must import — `go mod why go.opentelemetry.io/otel/trace` resolves
   through `internal/platform/logger`. It ships in the same v1.39.0
   release train as the other three, so the pin and the no-contrib rule
   are honored; four modules, one release. `go mod tidy` also pulls
   `cenkalti/backoff/v5`, `grpc-gateway/v2`, and `go.opentelemetry.io/proto/otlp`
   as INDIRECT requires of the exporter (plus `otel/exporters/otlp/otlptrace`,
   the exporter's parent module). **Zero contrib code is built**:
   `go mod why go.opentelemetry.io/contrib/detectors/gcp` → "main module
   does not need package" (it is only a module-graph member via
   prometheus/common). Pin rationale verified: v1.39.0 was already the MVS
   floor in go.sum (go.sum:115-124) and all four modules carry
   `go 1.24.0` (`go list -m -f '{{.GoVersion}}' …` → `1.24.0`), matching
   the repo's `go 1.24.0` (go.mod:3) and the `golang:1.24-alpine`
   service.Dockerfile toolchain — the dev-note 055 trap does not apply.
2. **Synthetic remote parent for the publisher (no migration):** the
   outbox stores only `trace_id` — no span id (docs/schema/procurement.md,
   the #31 schema delta; migration 0006:28) — and the loop runs from the
   worker's root context, so there is no real parent to continue.
   `ContextWithTraceIDString` synthesizes a remote, sampled parent with a
   deterministic NONZERO span id derived from the trace id's second half
   (a zero span id would make the SpanContext invalid and silently start a
   NEW trace). The publish span and the message's `traceparent` therefore
   carry the outbox row's trace id — the "one PO flow = one trace" demo
   survives the broker hop. Trade-off: Jaeger shows a parent node with no
   recorded span (the outbox row itself was never a span); accepted in
   exchange for zero schema migration. An existing valid span in ctx is
   never overridden.
3. **Legacy continuity without a flag:** the gateway derives
   `X-Trace-Id`/`x-trace-id` FROM the span, so pre-#54 clients that sent
   `X-Trace-Id` keep bridging (a hex32 value anchors the same trace; any
   other value is replaced by the generated id — the values can never
   disagree again). On the gRPC side the legacy `x-trace-id` metadata is
   only consulted when no traceparent is present, and the identity/worker
   paths keep `logger.TraceID` fallbacks for span-less contexts.
4. **pgx tracer on the three RUNTIME pools only; the boot migration pools
   are excluded** (procurement + identity each open a second
   simple-protocol pool for migrations): migrations run once per deploy,
   before the service serves any request, carry no request context (their
   spans would all be unrelated root traces), and the simple protocol
   would emit one span per multi-statement script. Cost of the exclusion:
   schema migrations are not traced — deliberate.
5. **Mongo spans deferred** (maintainer decision, issue #54 comment 2):
   `mongo.CommandMonitor` events carry no `context.Context`, so parent
   linkage would be limited and the spans would not join request traces.
   Same deferral pattern as dev-notes 045/047; revisit if the driver adds
   context-carrying hooks.
6. **No sampling env knob** (maintainer decision): `ParentBased(AlwaysSample)`
   is hard-coded in `telemetry.Setup`. Demo-scale traffic is tiny; Jaeger
   needs complete traces for the §22 demo.
7. **Jaeger compose details:** image pinned `jaegertracing/all-in-one:1.62.0`
   (repo convention pins exact tags: prometheus v3.6.0, grafana 12.1.0,
   collector 0.111.0); only the UI port is published — the OTLP 4317
   receiver stays container-internal (the METRICS_PORT pattern; only the
   collector exports to it). **No healthcheck:** the modern all-in-one
   image ships no shell/wget, so a `CMD-SHELL` probe cannot run;
   `docker compose up --wait` treats a running container without a
   healthcheck as healthy — reachability is verified via the UI/API
   instead (evidence below).
8. **Scope follows the issue text:** DLQ alerting rules are OUT (maintainer
   decision — separate issue); the collector is NOT profiled (it was a
   default service since M0; Jaeger/Prometheus/Grafana remain opt-in under
   `observability` — the #55 profile deviation stands, so without the
   profile the collector logs export errors while `debug` still prints
   spans).
9. **Interceptor order:** OTel FIRST, then the legacy trace interceptor,
   then metrics/RBAC/logging/recovery. The span context must exist before
   any consumer; the legacy interceptor still writes its context value for
   span-less fallbacks. Locked by `TestOTelInterceptorFirstSpanWinsOverLegacyTraceID`.

## Evidence

- `go vet ./...` — clean; `go build ./...` — clean.
- `go test ./internal/... ./services/...` — all packages `ok`
  (DB/AMQP-gated tests skip as before). New/updated locking tests:
  `internal/platform/telemetry` (16 tests), `internal/platform/logger`
  (`TestTraceIDSpanFirst`), `internal/platform/grpcserver`
  (`TestOTelInterceptorFirstSpanWinsOverLegacyTraceID`),
  `services/worker/publisher` (`TestPublishContextLinksOutboxRowTrace`),
  `services/worker/consumer` (`TestTraceContext`),
  `services/gateway/server` (`TestForwardContextCarriesRequestSpan`,
  updated `TestApprovalDecideTracePropagation`).
- `docker compose config` — valid; with `COMPOSE_PROFILES=observability`
  the service set includes `jaeger` (image
  `jaegertracing/all-in-one:1.62.0`, UI 16686, no healthcheck) and all
  five app services carry `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317`.
- `docker compose --profile observability up -d --wait --build` — all
  services healthy on the rebuilt images (golang:1.24-alpine toolchain
  builds clean with the v1.39.0 modules; jaeger up without a healthcheck).
- **"One PO flow = one trace" demonstrated live** (the §22 demo): driving
  requisition create → submit → approve → PO create → PO issue through the
  REST edge with an inbound `traceparent` produced trace
  `5aa7651916cd43dd8448eb211c80319c` in Jaeger
  (`GET http://localhost:16686/api/traces/<id>`) with **149 spans across
  all four Go services**: `http *` (gateway), `/procurement.v1.*` +
  `/documents.v1.*` client/server pairs (gateway ↔ procurement/documents),
  every `pgx BEGIN/SELECT/INSERT/UPDATE/COMMIT` (procurement pool),
  `outbox publish pr.submitted/pr.approved/pr.step_approved/po.created/po.issued`
  (worker — the synthetic parent linkage), and
  `amqp consume documents.history/notifications` (worker consumers, joined
  via the message headers).
- **Log trace_id = span trace id** (issue AC 2), live comparison: worker
  `outbox event published` lines, gateway `http request` lines, and
  procurement `grpc request` lines all carry
  `"trace_id":"5aa7651916cd43dd8448eb211c80319c"` — byte-identical to the
  Jaeger trace id above.
- **Worker's own pool traced**: `GET /api/traces?service=worker&operation=pgx SELECT`
  returns root traces (the publisher loop polls off the root context — no
  request parent exists), confirming the pgx tracer on the worker pool's
  outbox Pending/MarkPublished queries.

## Follow-ups

- Mongo spans stay deferred (maintainer decision); revisit with
  context-carrying driver hooks.
- DLQ alerting rules: separate issue (maintainer decision).
- The `debug` exporter also prints traces to the collector log; drop it
  once the Jaeger path is trusted in demos, if log volume matters.
- `apps/web` (Next.js) does not propagate traceparent to the gateway yet —
  browser → BFF spans are out of #54's Go scope.
