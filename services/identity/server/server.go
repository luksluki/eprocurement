// Package server wires the identity gRPC server: standard health service,
// reflection, and unary interceptors for trace-id propagation, structured
// logging, and panic recovery.
//
// The identity.v1 API (Login, Refresh, ListUsers, ...) arrives with the auth
// issues; this skeleton exposes grpc.health.v1 so orchestration can observe
// liveness (service "") and readiness (service "identity", backed by a
// PostgreSQL ping loop).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/identity/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// ServiceName is the health-check service name whose status tracks PostgreSQL
// readiness; the empty service name ("") always reports SERVING while the
// process is up.
const ServiceName = "identity"

// Pinger reports whether the service's dependencies are reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server owns the identity gRPC server and its readiness loop.
type Server struct {
	grpc     *grpc.Server
	health   *health.Server
	log      *slog.Logger
	pinger   Pinger
	interval time.Duration
	// lastHealth mirrors the reported readiness status; written only by
	// refreshReadiness, which runs sequentially (once in Serve, then from the
	// single readinessLoop goroutine).
	lastHealth  healthpb.HealthCheckResponse_ServingStatus
	identityAPI *api
	stop        chan struct{}
	done        chan struct{}
}

// New returns a Server ready to Serve on the given listener.
func New(log *slog.Logger, pinger Pinger, opts ...Option) *Server {
	s := &Server{
		log:      log,
		pinger:   pinger,
		interval: 10 * time.Second,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}

	s.grpc = grpc.NewServer(grpc.ChainUnaryInterceptor(
		traceInterceptor,
		s.loggingInterceptor,
		s.recoveryInterceptor,
	))
	s.health = health.NewServer()
	s.lastHealth = healthpb.HealthCheckResponse_NOT_SERVING
	s.health.SetServingStatus(ServiceName, s.lastHealth) // until the first ping succeeds
	healthpb.RegisterHealthServer(s.grpc, s.health)
	if s.identityAPI != nil {
		identityv1.RegisterIdentityServer(s.grpc, s.identityAPI)
	}
	reflection.Register(s.grpc)
	return s
}

// Option customizes the Server.
type Option func(*Server)

// WithHealthInterval overrides the readiness ping interval (tests use short values).
func WithHealthInterval(d time.Duration) Option {
	return func(s *Server) { s.interval = d }
}

// WithIdentityAPI registers the identity.v1 API (login/refresh/logout).
func WithIdentityAPI(svc *auth.Service) Option {
	return func(s *Server) { s.identityAPI = newAPI(s.log, svc) }
}

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

// refreshReadiness flips the identity health status to match the latest ping.
func (s *Server) refreshReadiness(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := s.pinger.Ping(pingCtx)
	cancel()

	st := healthpb.HealthCheckResponse_SERVING
	if err != nil {
		st = healthpb.HealthCheckResponse_NOT_SERVING
	}
	if s.lastHealth != st {
		s.health.SetServingStatus(ServiceName, st)
		s.lastHealth = st
	}
	if err != nil {
		s.log.Warn("dependency health check failed", "error", err.Error(), "dependency", "postgres")
	}
}

// traceInterceptor propagates the inbound trace id from gRPC metadata into the
// request context, generating one when the caller did not send it.
func traceInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
