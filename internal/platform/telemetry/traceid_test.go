package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// AC #2 (linkage): the outbox stores only a trace id (no span id), so the
// publisher's parent must be synthesized with a NONZERO span id — a zero
// SpanID makes the SpanContext invalid and would silently start a new
// trace, breaking "one PO flow = one trace".
func TestContextWithTraceIDString(t *testing.T) {
	const tid = "0af7651916cd43dd8448eb211c80319c"

	ctx := ContextWithTraceIDString(context.Background(), tid)
	sc := spanContextOf(ctx)
	if !sc.IsValid() {
		t.Fatal("span context invalid after synthesizing a valid hex32 id")
	}
	if got := sc.TraceID().String(); got != tid {
		t.Fatalf("trace id = %q, want the envelope id %q (a new trace would break linkage)", got, tid)
	}
	if sc.SpanID() == (trace.SpanID{}) {
		t.Fatal("span id is zero: SpanContext would be invalid — the exact bug the helper prevents")
	}
	if !sc.IsSampled() {
		t.Fatal("synthesized parent must be sampled so the publish span is exported")
	}
	if !sc.IsRemote() {
		t.Fatal("synthesized parent must be marked remote")
	}

	again := ContextWithTraceIDString(context.Background(), tid)
	if spanContextOf(again).SpanID() != sc.SpanID() {
		t.Fatal("span id not deterministic: same outbox trace id must yield the same parent")
	}
}

func TestContextWithTraceIDStringInvalidInput(t *testing.T) {
	cases := []struct {
		name, id string
	}{
		{"empty", ""},
		{"too short", "0af7651916cd43dd"},
		{"not hex", "zzf7651916cd43dd8448eb211c80319c"},
		{"too long", "0af7651916cd43dd8448eb211c80319c00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ContextWithTraceIDString(context.Background(), tc.id)
			if spanContextOf(ctx).IsValid() {
				t.Fatalf("invalid id %q synthesized a span context; ctx must stay unchanged", tc.id)
			}
		})
	}
}

// The zero-second-half corner case still yields a valid (nonzero) span id.
func TestContextWithTraceIDStringZeroSpanHalf(t *testing.T) {
	ctx := ContextWithTraceIDString(context.Background(), "ffffffffffffffff0000000000000000")
	sc := spanContextOf(ctx)
	if !sc.IsValid() || sc.SpanID() == (trace.SpanID{}) {
		t.Fatalf("all-zero span-id half must fall back to a nonzero id, got %#v", sc.SpanID())
	}
}

// An existing valid span context always wins — the helper never re-parents
// a request that already carries real trace context.
func TestContextWithTraceIDStringDoesNotOverrideExistingSpan(t *testing.T) {
	ctx, existing := testTracer.Start(context.Background(), "existing")
	defer existing.End()
	want := spanContextOf(ctx)

	ctx = ContextWithTraceIDString(ctx, "0af7651916cd43dd8448eb211c80319c")
	if got := spanContextOf(ctx); got.TraceID() != want.TraceID() || got.SpanID() != want.SpanID() {
		t.Fatalf("synthesis overrode an existing span context: got %v, want %v", got, want)
	}
}
