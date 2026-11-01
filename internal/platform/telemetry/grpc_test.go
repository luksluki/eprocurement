package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// AC #3 (REST → gRPC hand-off): the client interceptor must inject the
// W3C traceparent of the CURRENT span into the outgoing metadata — next to
// the claim headers forwardContext already attached, not replacing them.
func TestClientInterceptorInjectsTraceparent(t *testing.T) {
	ctx, span := testTracer.Start(context.Background(), "http GET /api/v1/vendors")
	defer span.End()
	want := spanContextOf(ctx).TraceID().String()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-user-id", "u-1", "x-tenant-id", "t-1")

	var captured context.Context
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		captured = ctx
		return nil
	}
	if err := GRPCUnaryClientInterceptor()(ctx, "/procurement.v1.Procurement/ListVendors",
		nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	md, ok := metadata.FromOutgoingContext(captured)
	if !ok {
		t.Fatal("captured context carries no outgoing metadata")
	}
	tp := md.Get("traceparent")
	if len(tp) != 1 {
		t.Fatalf("traceparent metadata = %v, want exactly one value", tp)
	}
	// W3C format: version-traceid-spanid-flags; the trace id MUST be the
	// caller's span trace id, not a fresh one.
	if got := tp[0][3:35]; got != want {
		t.Fatalf("traceparent trace id = %q, want the request span's %q", got, want)
	}
	for _, legacy := range md.Get("x-user-id") {
		if legacy != "u-1" {
			t.Fatalf("claim metadata disturbed: x-user-id = %v", md.Get("x-user-id"))
		}
	}
	if len(md.Get("x-tenant-id")) == 0 {
		t.Fatal("x-user-id/x-tenant-id claim metadata lost by injection")
	}
}

// AC #3 (span-first): the server interceptor extracts traceparent and the
// legacy x-trace-id NEVER overrides it when both are present with
// different values.
func TestServerInterceptorPrefersTraceparentOverLegacyHeader(t *testing.T) {
	ctx, span := testTracer.Start(context.Background(), "client span")
	defer span.End()
	want := spanContextOf(ctx).TraceID().String()

	in := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"traceparent", "00-"+want+"-"+spanContextOf(ctx).SpanID().String()+"-01",
		"x-trace-id", "ffffffffffffffffffffffffffffffff"))

	var handlerCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCtx = ctx
		return nil, nil
	}
	if _, err := GRPCUnaryServerInterceptor()(in, nil, &grpc.UnaryServerInfo{
		FullMethod: "/procurement.v1.Procurement/ListVendors"}, handler); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	if got := spanContextOf(handlerCtx).TraceID().String(); got != want {
		t.Fatalf("server span trace id = %q, want traceparent's %q (legacy x-trace-id must not win)", got, want)
	}
}

// Legacy bridge: with ONLY x-trace-id (a pre-#54 gateway), the same id
// becomes the trace root instead of a fresh unrelated trace.
func TestServerInterceptorBridgesLegacyXTraceID(t *testing.T) {
	const legacy = "0af7651916cd43dd8448eb211c80319c"
	in := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-trace-id", legacy))

	var handlerCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) { handlerCtx = ctx; return nil, nil }
	_, _ = GRPCUnaryServerInterceptor()(in, nil, &grpc.UnaryServerInfo{
		FullMethod: "/procurement.v1.Procurement/ListVendors"}, handler)

	if got := spanContextOf(handlerCtx).TraceID().String(); got != legacy {
		t.Fatalf("server span trace id = %q, want legacy %q", got, legacy)
	}
}

// Health/reflection methods stay span-free so compose healthchecks do not
// flood Jaeger.
func TestServerInterceptorSkipsHealthAndReflection(t *testing.T) {
	for _, method := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		in := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"traceparent", "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01"))
		var handlerCtx context.Context
		handler := func(ctx context.Context, req any) (any, error) { handlerCtx = ctx; return nil, nil }
		_, _ = GRPCUnaryServerInterceptor()(in, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		if spanContextOf(handlerCtx).IsValid() {
			t.Fatalf("%s must not carry a span context (healthcheck noise)", method)
		}
	}
}

// End-to-end leg: what the client interceptor injects, the server
// interceptor extracts — the trace id survives the hop.
func TestGRPCClientServerRoundTrip(t *testing.T) {
	// A local provider keeps this test independent of the global one.
	local := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = local.Shutdown(context.Background()) }()
	ctx, span := local.Tracer("local").Start(context.Background(), "origin")
	defer span.End()
	want := spanContextOf(ctx).TraceID().String()

	var wire metadata.MD
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		wire, _ = metadata.FromOutgoingContext(ctx)
		return nil
	}
	_ = GRPCUnaryClientInterceptor()(ctx, "/pkg.Svc/M", nil, nil, nil, invoker)

	var handlerCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) { handlerCtx = ctx; return nil, nil }
	in := metadata.NewIncomingContext(context.Background(), wire)
	_, _ = GRPCUnaryServerInterceptor()(in, nil, &grpc.UnaryServerInfo{FullMethod: "/pkg.Svc/M"}, handler)

	if got := spanContextOf(handlerCtx).TraceID().String(); got != want {
		t.Fatalf("trace id changed across the gRPC leg: got %q, want %q", got, want)
	}
}
