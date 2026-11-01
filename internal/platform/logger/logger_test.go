package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil)).With("service", "test")
}

func TestFromContextIncludesTraceID(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)

	FromContext(WithTraceID(context.Background(), "abc123"), log).Info("hello")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if got := record["trace_id"]; got != "abc123" {
		t.Fatalf("trace_id = %v, want abc123", got)
	}
	if got := record["msg"]; got != "hello" {
		t.Fatalf("msg = %v, want hello", got)
	}
}

func TestFromContextOmitsTraceIDWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)

	FromContext(context.Background(), log).Info("hello")

	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("log line should not contain trace_id: %s", buf.String())
	}
}

func TestTraceIDRoundTrip(t *testing.T) {
	ctx := WithTraceID(context.Background(), "tid-1")
	if got := TraceID(ctx); got != "tid-1" {
		t.Fatalf("TraceID = %q, want tid-1", got)
	}
	if got := TraceID(context.Background()); got != "" {
		t.Fatalf("TraceID on empty context = %q, want empty", got)
	}
}

// AC #54 (span-first): when the context carries an OpenTelemetry span
// context, TraceID reports the SPAN's trace id — the value Jaeger shows —
// even if a legacy trace-id value also rides the context; without a span
// the legacy value still answers (cron/boot paths keep logging).
func TestTraceIDSpanFirst(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := tp.Tracer("test").Start(WithTraceID(context.Background(), "legacy-id"), "op")
	defer span.End()
	want := span.SpanContext().TraceID().String()

	if got := TraceID(ctx); got != want {
		t.Fatalf("TraceID = %q, want the span's %q (the legacy value must not win)", got, want)
	}
	if got := TraceID(WithTraceID(context.Background(), "legacy-only")); got != "legacy-only" {
		t.Fatalf("TraceID without a span = %q, want the legacy value", got)
	}
}

func TestNewTraceIDShape(t *testing.T) {
	id := NewTraceID()
	if len(id) != 32 { // 16 bytes hex-encoded
		t.Fatalf("NewTraceID length = %d, want 32", len(id))
	}
	if NewTraceID() == id {
		t.Fatal("NewTraceID returned the same value twice")
	}
}
