package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestMain installs an SDK TracerProvider WITHOUT a span processor: spans
// get real, sampled trace ids (exactly what the propagation and linkage
// assertions need) but nothing is exported, so the package tests run
// offline and quiet.
func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample()))))
	m.Run()
}

// AC #1: the SDK activates in every service; an empty OTLP endpoint keeps
// the SDK on (real trace ids keep log/outbox linkage) without an exporter.
func TestSetupEmptyEndpointKeepsSDKActive(t *testing.T) {
	tp, err := Setup(context.Background(), "test-svc", "local", "")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer Shutdown(context.Background(), tp, 0)

	_, span := otel.Tracer("test").Start(context.Background(), "root")
	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("span context invalid after Setup with empty endpoint; SDK must stay active")
	}
	if !sc.IsSampled() {
		t.Fatal("root span not sampled; ParentBased(AlwaysSample) is the pinned sampler")
	}
	span.End()
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"scheme stripped", "http://otel-collector:4317", "otel-collector:4317"},
		{"https stripped", "https://collector.example:4317", "collector.example:4317"},
		{"bare host:port kept", "localhost:4317", "localhost:4317"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeEndpoint(tc.in); got != tc.want {
				t.Fatalf("NormalizeEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// spanContextOf returns the current span context of ctx (test helper).
func spanContextOf(ctx context.Context) trace.SpanContext {
	return trace.SpanContextFromContext(ctx)
}

// testTracer starts real (sampled, unexported) spans in tests.
var testTracer = otel.Tracer("github.com/luksluki/eprocurement/internal/platform/telemetry/test")
