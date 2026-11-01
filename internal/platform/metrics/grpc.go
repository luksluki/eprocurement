package metrics

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// GRPCUnaryInterceptor measures every unary call as
// grpc_request_duration_seconds{method, code} (#55, PRD §18). grpcserver
// chains it right after the trace interceptor, so the observed duration
// covers the RBAC re-check and the handler itself, and the code label is
// the final status the client sees (Internal after a recovered panic).
func (c *Collectors) GRPCUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		c.grpcDuration.WithLabelValues(info.FullMethod, status.Code(err).String()).Observe(time.Since(start).Seconds())
		return resp, err
	}
}
