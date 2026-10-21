package grpcserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakePinger flips on demand to drive readiness transitions.
type fakePinger struct {
	err error
}

func (p *fakePinger) Ping(ctx context.Context) error { return p.err }

func startServer(t *testing.T, pinger *fakePinger, logBuf *bytes.Buffer) (*grpc.ClientConn, func()) {
	t.Helper()

	log := slog.New(slog.NewJSONHandler(logBuf, nil)).With("service", "test")
	srv := New(log, pinger,
		WithServiceName("test-svc"),
		WithHealthInterval(50*time.Millisecond),
	)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(context.Background(), lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, func() {
		_ = conn.Close()
		_ = lis.Close()
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestOverallHealthServingWhileDependencyDown(t *testing.T) {
	pinger := &fakePinger{err: errors.New("db down")}
	var logBuf bytes.Buffer
	conn, cleanup := startServer(t, pinger, &logBuf)
	defer cleanup()

	client := healthpb.NewHealthClient(conn)

	// "" tracks process liveness: serving even though the dependency is down.
	resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("overall check: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("overall status = %s, want SERVING", resp.GetStatus())
	}

	// The named service tracks readiness and must be NOT_SERVING.
	resp, err = client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "test-svc"})
	if err != nil {
		t.Fatalf("readiness check: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("readiness = %s, want NOT_SERVING while dependency is down", resp.GetStatus())
	}

	// Recovery: once the dependency answers, readiness returns to SERVING.
	pinger.err = nil
	waitFor(t, 2*time.Second, func() bool {
		resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "test-svc"})
		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
	})
}

func TestReadinessServingWhenDependencyUp(t *testing.T) {
	var logBuf bytes.Buffer
	conn, cleanup := startServer(t, &fakePinger{}, &logBuf)
	defer cleanup()

	client := healthpb.NewHealthClient(conn)
	waitFor(t, 2*time.Second, func() bool {
		resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "test-svc"})
		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
	})
}

func TestTraceInterceptorUsesInboundTraceID(t *testing.T) {
	var ctx context.Context
	handler := func(ctx2 context.Context, req any) (any, error) {
		ctx = ctx2
		return nil, nil
	}

	inCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-trace-id", "inbound-trace-1"))
	if _, err := TraceInterceptor(inCtx, nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if got := logger.TraceID(ctx); got != "inbound-trace-1" {
		t.Fatalf("trace id = %q, want inbound-trace-1", got)
	}
}

func TestTraceInterceptorGeneratesTraceID(t *testing.T) {
	var ctx context.Context
	handler := func(ctx2 context.Context, req any) (any, error) {
		ctx = ctx2
		return nil, nil
	}

	if _, err := TraceInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if got := logger.TraceID(ctx); len(got) != 32 {
		t.Fatalf("generated trace id = %q, want 32 hex chars", got)
	}

	// Oversized inbound ids are replaced, not trusted verbatim.
	oversized := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-trace-id", "0123456789012345678901234567890123456789012345678901234567890123456789"))
	if _, err := TraceInterceptor(oversized, nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if got := logger.TraceID(ctx); len(got) != 32 {
		t.Fatalf("replaced trace id = %q, want 32 hex chars", got)
	}
}

func TestLoggingInterceptorLogsTraceIDAndCode(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	s := &Server{log: log}

	ctx := logger.WithTraceID(context.Background(), "trace-log-1")
	handlerErr := status.Error(codes.NotFound, "no such vendor")
	_, err := s.loggingInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/procurement.v1.Procurement/GetVendor"},
		func(context.Context, any) (any, error) { return nil, handlerErr })
	if !errors.Is(err, handlerErr) {
		t.Fatalf("interceptor must pass the handler error through, got %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal(logBuf.Bytes(), &record); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if got := record["trace_id"]; got != "trace-log-1" {
		t.Fatalf("trace_id = %v, want trace-log-1", got)
	}
	if got := record["code"]; got != "NotFound" {
		t.Fatalf("code = %v, want NotFound", got)
	}
}

func TestRecoveryInterceptorConvertsPanicToInternal(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	s := &Server{log: log}

	resp, err := s.recoveryInterceptor(logger.WithTraceID(context.Background(), "trace-panic"), nil,
		&grpc.UnaryServerInfo{FullMethod: "/procurement.v1.Procurement/Boom"},
		func(context.Context, any) (any, error) { panic("boom") })
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %s, want Internal", status.Code(err))
	}
	if logBuf.Len() == 0 {
		t.Fatal("panic was not logged")
	}
}
