// Package telemetry wires the OpenTelemetry SDK for all five Go services
// (issue #54; PRD §18 "trace context propagated REST → gRPC → RabbitMQ
// headers → worker; spans around DB, cache, publish/consume").
//
// The instrumentation is hand-rolled on the three core modules only —
// go.opentelemetry.io/otel, otel/sdk, and the OTLP gRPC trace exporter —
// with NO otel-contrib instrumentation packages: the HTTP (fiber), gRPC,
// pgx, go-redis, and AMQP integration points below are thin interceptors
// around the public APIs this repo already uses (#54 maintainer decision,
// issue comment: no contrib; pinned v1.39.0, disclosed in the PR body per
// golden rule 10 and the #49/#55 dependency precedent).
//
// Degradation contract: every helper is safe when Setup was never called
// (unit tests, CI). Without a registered TracerProvider the global tracer
// is a no-op that still PRESERVES an extracted or synthesized parent span
// context, so context propagation works even while nothing is exported.
package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// Propagator is the W3C tracecontext + baggage propagator (#54 maintainer
// decision, issue comment 2). It is installed globally by Setup and used
// directly by the helpers, so propagation behaves identically whether or
// not Setup ran (it also matches otel's own default).
var Propagator propagation.TextMapPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

// Setup builds the process-wide TracerProvider and installs it (plus the
// W3C propagator) as otel globals. Sampling is fixed — no env knob — at
// ParentBased(AlwaysSample): a trace started by the gateway is sampled all
// the way through gRPC, RabbitMQ, and the worker, which is the whole point
// of #54's "one PO flow = one trace" demo (#54 maintainer decision, issue
// comment 2).
//
// endpoint is OTEL_EXPORTER_OTLP_ENDPOINT (host:port, an http:// scheme is
// tolerated and stripped). An empty endpoint still activates the SDK —
// spans get real trace ids that keep log/outbox linkage intact — but no
// OTLP exporter is attached, so a bare local run or CI stays silent. The
// exporter targets the compose-network collector with TLS disabled (local
// plaintext); callers own the returned provider's Shutdown.
func Setup(ctx context.Context, service, env, endpoint string) (*sdktrace.TracerProvider, error) {
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithResource(sdkresource.NewSchemaless(
			semconv.ServiceName(service),
			attribute.String("deployment.environment", env),
		)),
	}
	if endpoint != "" {
		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(NormalizeEndpoint(endpoint)),
			otlptracegrpc.WithInsecure())
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(Propagator)
	return tp, nil
}

// NormalizeEndpoint strips an optional http(s):// scheme from an OTLP
// endpoint value — the OTel env convention allows URLs ("http://otel-
// collector:4317") while otlptracegrpc.WithEndpoint wants host:port.
func NormalizeEndpoint(endpoint string) string {
	if i := strings.Index(endpoint, "://"); i >= 0 {
		if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
			return u.Host
		}
		return strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	}
	return endpoint
}

// Shutdown flushes the provider within timeout. It is the deferred
// companion of Setup for the service mains.
func Shutdown(ctx context.Context, tp *sdktrace.TracerProvider, timeout time.Duration) {
	if tp == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = tp.Shutdown(shutdownCtx)
}
