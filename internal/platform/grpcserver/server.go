// Package grpcserver provides the shared gRPC server scaffolding for all
// services: standard health (liveness "" / readiness keyed by service name,
// backed by a dependency ping loop), reflection, and the platform unary
// interceptors — trace-id propagation, JSON request logging, and panic
// recovery. Services add their own interceptors (e.g. RBAC) and register
// their APIs on the underlying grpc.Server before Serve.
package grpcserver

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Pinger reports whether the service's dependencies are reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server owns a gRPC server and its readiness loop.
type Server struct {
	grpc     *grpc.Server
	health   *health.Server
	log      *slog.Logger
	pinger   Pinger
	interval time.Duration
	extra    []grpc.UnaryServerInterceptor
	// lastHealth mirrors the reported readiness status; written only by
	// refreshReadiness, which runs sequentially (once in Serve, then from the
	// single readinessLoop goroutine).
	lastHealth  healthpb.HealthCheckResponse_ServingStatus
	serviceName string
	stop        chan struct{}
	done        chan struct{}
}

// Option customizes the Server.
type Option func(*Server)

// WithHealthInterval overrides the readiness ping interval (tests use short values).
func WithHealthInterval(d time.Duration) Option {
	return func(s *Server) { s.interval = d }
}

// WithServiceName sets the health-check service name whose status tracks the
// dependency ping (readiness); "" always reports SERVING (liveness).
func WithServiceName(name string) Option {
	return func(s *Server) { s.serviceName = name }
}

// WithUnary appends interceptors after the trace interceptor and before
// logging — where the RBAC re-check belongs.
func WithUnary(interceptors ...grpc.UnaryServerInterceptor) Option {
	return func(s *Server) { s.extra = append(s.extra, interceptors...) }
}

// New returns a Server ready to accept API registrations and then Serve.
func New(log *slog.Logger, pinger Pinger, opts ...Option) *Server {
	s := &Server{
		log:         log,
		pinger:      pinger,
		interval:    10 * time.Second,
		serviceName: "service",
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}

	chain := append([]grpc.UnaryServerInterceptor{TraceInterceptor},
		append(s.extra, s.loggingInterceptor, s.recoveryInterceptor)...)
	s.grpc = grpc.NewServer(grpc.ChainUnaryInterceptor(chain...))
	s.health = health.NewServer()
	s.lastHealth = healthpb.HealthCheckResponse_NOT_SERVING
	s.health.SetServingStatus(s.serviceName, s.lastHealth) // until the first ping succeeds
	healthpb.RegisterHealthServer(s.grpc, s.health)
	reflection.Register(s.grpc)
	return s
}

// GRPC exposes the underlying server for API registration before Serve.
func (s *Server) GRPC() *grpc.Server { return s.grpc }

// Serve accepts connections until ctx is cancelled, then stops gracefully.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.grpc.Serve(lis) }()

	s.refreshReadiness(ctx)
	go s.readinessLoop(ctx)

	select {
	case err := <-errCh:
		close(s.stop)
		<-s.done
		return err
	case <-ctx.Done():
		s.log.Info("shutting down grpc server")
		stopped := make(chan struct{})
		go func() { s.grpc.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			s.grpc.Stop()
		}
		close(s.stop)
		<-s.done
		return nil
	}
}

func (s *Server) readinessLoop(ctx context.Context) {
	defer close(s.done)
	if s.interval <= 0 { // tests: readiness set once in Serve, no ticking
		return
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.refreshReadiness(ctx)
		}
	}
}

// refreshReadiness flips the service's health status to match the latest ping.
func (s *Server) refreshReadiness(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := s.pinger.Ping(pingCtx)
	cancel()

	st := healthpb.HealthCheckResponse_SERVING
	if err != nil {
		st = healthpb.HealthCheckResponse_NOT_SERVING
	}
	if s.lastHealth != st {
		s.health.SetServingStatus(s.serviceName, st)
		s.lastHealth = st
	}
	if err != nil {
		s.log.Warn("dependency health check failed", "error", err.Error(), "dependency", "postgres")
	}
}

// TraceInterceptor propagates the inbound trace id from gRPC metadata into
// the request context, generating one when the caller did not send it.
func TraceInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	traceID := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-trace-id"); len(vals) > 0 {
			traceID = strings.TrimSpace(vals[0])
		}
	}
	if traceID == "" || len(traceID) > 64 {
		traceID = logger.NewTraceID()
	}
	return handler(logger.WithTraceID(ctx, traceID), req)
}

func (s *Server) loggingInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	logger.FromContext(ctx, s.log).Info("grpc request",
		"method", info.FullMethod,
		"code", status.Code(err).String(),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return resp, err
}

func (s *Server) recoveryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx, s.log).Error("grpc handler panicked",
				"method", info.FullMethod,
				"panic", fmt.Sprint(r),
			)
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}
