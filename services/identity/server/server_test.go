package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// fakePinger flips on demand to drive readiness transitions.
type fakePinger struct {
	err error
}

func (p *fakePinger) Ping(ctx context.Context) error { return p.err }

func startServer(t *testing.T, pinger *fakePinger, logBuf *bytes.Buffer) (*grpc.ClientConn, func()) {
	t.Helper()

	log := slog.New(slog.NewJSONHandler(logBuf, nil)).With("service", "identity-test")
	srv := New(log, pinger, WithHealthInterval(50*time.Millisecond))

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

// The interceptors (trace/log/recovery) and the health mechanics they share
// are tested in internal/platform/grpcserver; these tests pin identity's
// wiring: liveness "" vs readiness "identity" on the platform server.
func TestOverallHealthServingWhileDependencyDown(t *testing.T) {
	pinger := &fakePinger{err: errors.New("db down")}
	var logBuf bytes.Buffer
	conn, cleanup := startServer(t, pinger, &logBuf)
	defer cleanup()

	client := healthpb.NewHealthClient(conn)

	// "" tracks process liveness: serving even though PostgreSQL is unreachable.
	resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("overall check: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("overall status = %s, want SERVING", resp.GetStatus())
	}

	// "identity" tracks readiness and must flip to NOT_SERVING.
	resp, err = client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: ServiceName})
	if err != nil {
		t.Fatalf("identity check: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("identity status = %s, want NOT_SERVING while dependency is down", resp.GetStatus())
	}

	// Recovery: once the dependency answers, readiness returns to SERVING.
	pinger.err = nil
	waitFor(t, 2*time.Second, func() bool {
		resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: ServiceName})
		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
	})
}

func TestReadinessServingWhenDependencyUp(t *testing.T) {
	var logBuf bytes.Buffer
	conn, cleanup := startServer(t, &fakePinger{}, &logBuf)
	defer cleanup()

	client := healthpb.NewHealthClient(conn)
	waitFor(t, 2*time.Second, func() bool {
		resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: ServiceName})
		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
	})
}
