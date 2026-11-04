package telemetry

import (
	"context"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// amqpTracer names the instrumentation scope of the RabbitMQ publish and
// consume spans.
var amqpTracer = otel.Tracer("github.com/luksluki/eprocurement/internal/platform/telemetry/amqp")

// TraceparentHeader is the AMQP application-header key carrying the W3C
// traceparent value across the broker (PRD §18 "trace context propagated
// ... → RabbitMQ headers → worker"). The retry ladder copies headers
// verbatim, so a retried delivery keeps its trace context for free.
const TraceparentHeader = "traceparent"

// StartPublishSpan starts a producer span for one outgoing message. The
// caller (the #45 outbox publisher) synthesizes the parent from the
// outbox row's trace_id first — see ContextWithTraceIDString — then calls
// this, then InjectAMQP.
func StartPublishSpan(ctx context.Context, routingKey string) (context.Context, trace.Span) {
	return amqpTracer.Start(ctx, "outbox publish "+routingKey,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", routingKey),
		))
}

// StartConsumeSpan starts a consumer span for one delivery, parented by
// whatever ExtractAMQP (or the ContextWithTraceIDString fallback for
// pre-#54 messages) put into ctx.
func StartConsumeSpan(ctx context.Context, queue string) (context.Context, trace.Span) {
	return amqpTracer.Start(ctx, "amqp consume "+queue,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", queue),
		))
}

// InjectAMQP injects the W3C traceparent (+ baggage) of ctx into the AMQP
// application headers. It is a no-op when ctx has no valid span context
// (the propagator skips invalid contexts), so old call sites degrade
// safely.
func InjectAMQP(ctx context.Context, headers amqp.Table) {
	Propagator.Inject(ctx, tableCarrier{t: headers})
}

// ExtractAMQP returns ctx with the trace context carried by the delivery
// headers, or ctx unchanged when they carry none.
func ExtractAMQP(ctx context.Context, headers amqp.Table) context.Context {
	return Propagator.Extract(ctx, tableCarrier{t: headers})
}

// tableCarrier adapts amqp.Table (map[string]any) to
// propagation.TextMapCarrier for the W3C propagators.
type tableCarrier struct{ t amqp.Table }

func (c tableCarrier) Get(key string) string {
	if v, ok := c.t[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (c tableCarrier) Set(key, value string) { c.t[key] = value }

func (c tableCarrier) Keys() []string {
	out := make([]string, 0, len(c.t))
	for k := range c.t {
		out = append(out, k)
	}
	return out
}
