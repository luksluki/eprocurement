package telemetry

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/luksluki/eprocurement/internal/platform/logger"
)

// httpTracer names the instrumentation scope of the gateway HTTP middleware.
var httpTracer = otel.Tracer("github.com/luksluki/eprocurement/internal/platform/telemetry/http")

// HTTPTrace is the gateway's edge middleware (#54): it extracts the inbound
// W3C traceparent, starts the server span, and — the load-bearing line —
// installs the span context into the fiber request context with
// c.SetUserContext. forwardContext builds the outgoing gRPC context from
// c.UserContext(), so the span rides into the gRPC client interceptor,
// which injects traceparent into the metadata: that is the REST → gRPC
// hand-off that keeps one PO flow one trace (PRD §18, §22 demo).
//
// Legacy continuity: the X-Trace-Id response header and the "trace_id"
// local are DERIVED from the span, so the REST-visible trace id, the
// x-trace-id metadata, the outbox trace_id column, and Jaeger can never
// disagree. An inbound X-Trace-Id without traceparent is honored as the
// trace root (same hex32 id), so pre-#54 callers keep their id; with
// neither header the span (or, without a configured SDK, the legacy
// generator) mints a fresh one.
//
// /healthz is span-free on purpose: the compose healthcheck ticks every
// 10 s and does not belong in Jaeger.
func HTTPTrace() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if c.Path() == "/healthz" {
			return c.Next()
		}

		ctx := Propagator.Extract(c.UserContext(), fiberCarrier{c: c})
		if !trace.SpanContextFromContext(ctx).IsValid() {
			if id := strings.TrimSpace(c.Get("X-Trace-Id")); isHex32(id) {
				ctx = ContextWithTraceIDString(ctx, id)
			}
		}
		ctx, span := httpTracer.Start(ctx, "http "+c.Method(),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Method()),
				attribute.String("url.path", c.Path()),
			))
		defer span.End()
		c.SetUserContext(ctx)

		id := trace.TraceID{}
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			id = sc.TraceID()
		}
		traceID := id.String()
		if !id.IsValid() {
			traceID = logger.NewTraceID() // no SDK configured: keep the legacy contract
		}
		c.Locals("trace_id", traceID)
		c.Set("X-Trace-Id", traceID)

		err := c.Next()
		if err != nil {
			span.SetStatus(codes.Error, "request failed")
		} else {
			span.SetAttributes(attribute.Int("http.response.status_code", c.Response().StatusCode()))
		}
		return err
	}
}

// isHex32 reports whether s is exactly 32 hex characters — the shape of
// both a W3C trace id and this repo's legacy X-Trace-Id.
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// fiberCarrier adapts the fiber request headers for propagation EXTRACT
// (the only direction the server middleware uses; c.Get is
// case-insensitive like HTTP headers).
type fiberCarrier struct{ c *fiber.Ctx }

func (h fiberCarrier) Get(key string) string { return h.c.Get(key) }

// Set writes the RESPONSE header — extraction never calls it; kept to
// satisfy propagation.TextMapCarrier.
func (h fiberCarrier) Set(key, value string) { h.c.Set(key, value) }

func (h fiberCarrier) Keys() []string {
	out := make([]string, 0, len(h.c.GetReqHeaders()))
	for k := range h.c.GetReqHeaders() {
		out = append(out, k)
	}
	return out
}
