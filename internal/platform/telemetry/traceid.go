package telemetry

import (
	"context"
	"encoding/hex"

	"go.opentelemetry.io/otel/trace"
)

// ContextWithTraceIDString synthesizes a REMOTE parent SpanContext from a
// plain 32-hex-char trace id (#54) and returns a context carrying it. It
// is the linkage for producers that know only a trace id — the outbox
// table stores trace_id with no span id (docs/schema/procurement.md, the
// #31 schema delta), and the worker publisher runs off the root context
// with no inbound span, so the "outbox publish" span needs a parent
// manufactured from the row: TraceID verbatim, plus a deterministic
// NON-ZERO SpanID derived from the trace id's second half (a zero SpanID
// makes the SpanContext invalid, which would silently start a new trace —
// the exact failure this helper exists to prevent). The same fallback
// covers consumers draining messages published before #54.
//
// The synthesized parent is remote and sampled, so the first real span
// under it keeps the outbox row's trace id and the whole PO flow shows as
// ONE trace in Jaeger. Trade-off (documented in the #54 dev note): the
// synthetic parent span itself never exists as a span — it only anchors
// the trace. When ctx already carries a valid span context, or the id is
// not exactly 32 hex chars, ctx is returned unchanged (no new trace is
// invented; an invalid parent degrades to a fresh root span).
func ContextWithTraceIDString(ctx context.Context, hexID string) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx // an existing span wins; never override with a guess
	}
	var tid trace.TraceID
	b, err := hex.DecodeString(hexID)
	if err != nil || len(b) != 16 {
		return ctx
	}
	copy(tid[:], b)

	// Deterministic nonzero span id from the trace id's random half. The
	// zero SpanID corner case falls back to a fixed nonzero value so the
	// SpanContext is always valid.
	var sid trace.SpanID
	copy(sid[:], tid[8:])
	if !sid.IsValid() {
		sid = trace.SpanID{1}
	}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}
