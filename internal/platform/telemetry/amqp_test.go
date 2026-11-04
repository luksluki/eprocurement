package telemetry

import (
	"context"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

// AC #2/#3 (MQ leg): the publisher synthesizes the parent from the outbox
// trace id, starts the producer span, and the injected traceparent carries
// the ROW's trace id — never a fresh one.
func TestPublishSpanCarriesOutboxTraceID(t *testing.T) {
	const rowTraceID = "0af7651916cd43dd8448eb211c80319c"
	ctx := ContextWithTraceIDString(context.Background(), rowTraceID)
	ctx, span := StartPublishSpan(ctx, "po.issued")
	defer span.End()

	if got := spanContextOf(ctx).TraceID().String(); got != rowTraceID {
		t.Fatalf("publish span trace id = %q, want the outbox row's %q", got, rowTraceID)
	}

	headers := amqp.Table{}
	InjectAMQP(ctx, headers)
	tp, _ := headers[TraceparentHeader].(string)
	if len(tp) != 55 || tp[0:2] != "00" || tp[3:35] != rowTraceID {
		t.Fatalf("traceparent header = %q, want 00-%s-<16 hex>-<2 hex>", tp, rowTraceID)
	}
}

// Extract side (consumer): what Publish injected, Extract restores.
func TestAMQPInjectExtractRoundTrip(t *testing.T) {
	parent, span := testTracer.Start(context.Background(), "outbox publish po.issued")
	defer span.End()
	want := spanContextOf(parent).TraceID().String()

	headers := amqp.Table{}
	InjectAMQP(parent, headers)
	if _, ok := headers[TraceparentHeader]; !ok {
		t.Fatal("traceparent header missing after Inject")
	}

	ctx := ExtractAMQP(context.Background(), headers)
	if got := spanContextOf(ctx).TraceID().String(); got != want {
		t.Fatalf("extracted trace id = %q, want %q", got, want)
	}
}

// Degradation: no span in ctx → no header (old publishers keep working;
// the consumer falls back to the envelope trace_id instead).
func TestInjectAMQPWithoutSpanIsNoop(t *testing.T) {
	headers := amqp.Table{}
	InjectAMQP(context.Background(), headers)
	if _, ok := headers[TraceparentHeader]; ok {
		t.Fatal("traceparent injected without a span context")
	}

	// Non-string and unrelated headers survive extraction untouched.
	in := amqp.Table{"x-retry-count": int32(2)}
	if got := ExtractAMQP(context.Background(), in); spanContextOf(got).IsValid() {
		t.Fatal("non-trace headers produced a span context")
	}
}
