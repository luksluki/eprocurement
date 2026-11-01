package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// grpcTracer names the instrumentation scope for the hand-rolled gRPC
// interceptors. Without Setup it is otel's global no-op tracer, which
// still forwards any extracted/synthesized parent span context.
var grpcTracer = otel.Tracer("github.com/luksluki/eprocurement/internal/platform/telemetry/grpc")

// GRPCUnaryClientInterceptor starts a client span for every unary call and
// injects the W3C traceparent (+ baggage) into the outgoing gRPC metadata,
// so the trace crosses the gateway → identity/procurement/documents hops
// (PRD §18). The span context comes from ctx — the gateway HTTP middleware
// stores it with c.SetUserContext, and forwardContext builds the outgoing
// ctx from exactly that, which is what makes the whole REST flow one trace.
func GRPCUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, span := grpcTracer.Start(ctx, method,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("rpc.system", "grpc")))
		defer span.End()

		// Inject on a COPY of the outgoing metadata: forwardContext already
		// attached claim headers (x-user-id/...); traceparent must join them,
		// not replace them.
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.MD{}
		} else {
			md = md.Copy()
		}
		Propagator.Inject(ctx, mdCarrier{md: &md})
		ctx = metadata.NewOutgoingContext(ctx, md)

		err := invoker(ctx, method, req, reply, cc, opts...)
		recordGRPCStatus(span, err)
		return err
	}
}

// GRPCUnaryServerInterceptor is chained FIRST in grpcserver.New, ahead of
// the legacy TraceInterceptor: it extracts the W3C traceparent from the
// incoming metadata (falling back to the legacy x-trace-id as a
// synthesized remote parent when only that is present), starts the server
// span, and hands the span context down so logger.TraceID(ctx) — span
// first — reports the span's trace id no matter what x-trace-id carried.
//
// grpc.health.v1 and reflection methods skip the span on purpose: the
// compose healthchecks tick every 10 s per service and would otherwise
// bury Jaeger in noise traces, sample-everything notwithstanding.
func GRPCUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.") ||
			strings.HasPrefix(info.FullMethod, "/grpc.reflection.") {
			return handler(ctx, req)
		}

		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ctx = Propagator.Extract(ctx, mdCarrier{md: &md})
			if !trace.SpanContextFromContext(ctx).IsValid() {
				// Legacy callers (pre-#54 gateway) send only x-trace-id:
				// anchor the same trace id instead of starting a new trace.
				if vals := md.Get("x-trace-id"); len(vals) > 0 {
					ctx = ContextWithTraceIDString(ctx, strings.TrimSpace(vals[0]))
				}
			}
		}

		service, method := splitFullMethod(info.FullMethod)
		ctx, span := grpcTracer.Start(ctx, info.FullMethod,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("rpc.system", "grpc"),
				attribute.String("rpc.service", service),
				attribute.String("rpc.method", method),
			))
		defer span.End()

		resp, err := handler(ctx, req)
		recordGRPCStatus(span, err)
		return resp, err
	}
}

// recordGRPCStatus marks the span OK/ERROR from the gRPC status code.
func recordGRPCStatus(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, status.Code(err).String())
		span.RecordError(err)
		return
	}
	span.SetStatus(codes.Ok, "")
}

// splitFullMethod breaks "/pkg.Service/Method" into ("pkg.Service",
// "Method"); unparsable names return the whole string unchanged.
func splitFullMethod(full string) (service, method string) {
	s := strings.TrimPrefix(full, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// mdCarrier adapts gRPC metadata.MD to propagation.TextMapCarrier — the
// whole reason no contrib package is needed for the gRPC leg. Get reads
// the first value (W3C headers are single-valued), Set replaces.
type mdCarrier struct{ md *metadata.MD }

var _ propagation.TextMapCarrier = mdCarrier{}

func (c mdCarrier) Get(key string) string {
	if vals := c.md.Get(key); len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func (c mdCarrier) Set(key, value string) {
	c.md.Set(key, value)
}

func (c mdCarrier) Keys() []string {
	out := make([]string, 0, len(*c.md))
	for k := range *c.md {
		out = append(out, k)
	}
	return out
}
