package metrics

// Table-driven unit tests for the #55 metric surface: the /metrics HTTP
// server lifecycle, the HTTP and gRPC latency histograms, the worker
// gauge setters (including the honest-gap DeleteExportAge semantics), and
// the #115 DLQ dead-letter counter (zero pre-initialization + targeted
// increments).

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gatherFamily returns the named metric family from the private registry.
func gatherFamily(t *testing.T, c *Collectors, name string) *dto.MetricFamily {
	t.Helper()
	families, err := c.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("family %q not present in gather output", name)
	return nil
}

// findMetric locates the child whose labels match exactly.
func findMetric(t *testing.T, mf *dto.MetricFamily, want map[string]string) *dto.Metric {
	t.Helper()
	for _, m := range mf.GetMetric() {
		got := map[string]string{}
		for _, lp := range m.GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		if len(got) != len(want) {
			continue
		}
		match := true
		for k, v := range want {
			if got[k] != v {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	t.Fatalf("no %s sample with labels %v", mf.GetName(), want)
	return nil
}

func TestObserveHTTPRecordsRouteAndStatus(t *testing.T) {
	c := New()
	c.ObserveHTTP("GET", "/api/v1/vendors/:id", 200, 120*time.Millisecond)
	c.ObserveHTTP("GET", "/api/v1/vendors/:id", 200, 30*time.Millisecond)
	// Failure path: a 503 is recorded under its own status label, not lost.
	c.ObserveHTTP("POST", "/api/v1/auth/login", 503, 5*time.Millisecond)

	httpMF := gatherFamily(t, c, "http_request_duration_seconds")
	if got := findMetric(t, httpMF, map[string]string{
		"method": "GET", "route": "/api/v1/vendors/:id", "status": "200",
	}).GetHistogram().GetSampleCount(); got != 2 {
		t.Fatalf("GET 200 sample count = %d, want 2", got)
	}
	if got := findMetric(t, httpMF, map[string]string{
		"method": "POST", "route": "/api/v1/auth/login", "status": "503",
	}).GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("POST 503 sample count = %d, want 1", got)
	}
}

func TestGRPCUnaryInterceptorRecordsMethodAndCode(t *testing.T) {
	c := New()
	ic := c.GRPCUnaryInterceptor()

	handlerErr := status.Error(codes.NotFound, "no such vendor")
	if _, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/procurement.v1.Procurement/GetVendor"},
		func(context.Context, any) (any, error) { return nil, handlerErr }); status.Code(err) != codes.NotFound {
		t.Fatalf("interceptor must pass the handler error through, got %v", err)
	}
	if _, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/identity.v1.Identity/Login"},
		func(context.Context, any) (any, error) { return "ok", nil }); err != nil {
		t.Fatalf("interceptor ok call: %v", err)
	}

	grpcMF := gatherFamily(t, c, "grpc_request_duration_seconds")
	if got := findMetric(t, grpcMF, map[string]string{
		"method": "/procurement.v1.Procurement/GetVendor", "code": "NotFound",
	}).GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("NotFound sample count = %d, want 1", got)
	}
	if got := findMetric(t, grpcMF, map[string]string{
		"method": "/identity.v1.Identity/Login", "code": "OK",
	}).GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("OK sample count = %d, want 1", got)
	}
}

func TestWorkerGaugeSetters(t *testing.T) {
	c := New()
	c.SetOutboxLag(90 * time.Second)
	c.SetDLQDepth(7)
	c.SetExportAge("tenant-a", 26*time.Hour)

	if got := gatherFamily(t, c, "outbox_lag_seconds").GetMetric()[0].GetGauge().GetValue(); got != 90 {
		t.Fatalf("outbox_lag_seconds = %v, want 90", got)
	}
	if got := gatherFamily(t, c, "dlq_depth_messages").GetMetric()[0].GetGauge().GetValue(); got != 7 {
		t.Fatalf("dlq_depth_messages = %v, want 7", got)
	}
	ageMF := gatherFamily(t, c, "export_age_seconds")
	if got := findMetric(t, ageMF, map[string]string{"tenant": "tenant-a"}).GetGauge().GetValue(); got != 93600 {
		t.Fatalf("export_age_seconds{tenant-a} = %v, want 93600", got)
	}

	// The honest-gap contract: deleting a tenant with no readable heartbeat
	// removes the series entirely; a never-set tenant is a no-op; a later
	// Set recreates the series.
	c.DeleteExportAge("tenant-a")
	c.DeleteExportAge("never-seen")
	families, err := c.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "export_age_seconds" {
			t.Fatalf("export_age_seconds still present after delete: %v", mf.GetMetric())
		}
	}
	c.SetExportAge("tenant-a", time.Hour)
	if got := findMetric(t, gatherFamily(t, c, "export_age_seconds"),
		map[string]string{"tenant": "tenant-a"}).GetGauge().GetValue(); got != 3600 {
		t.Fatalf("export_age_seconds{tenant-a} after recreate = %v, want 3600", got)
	}
}

// TestDLQDeliveriesCounter pins the #115 pre-initialization contract:
// immediately after New() the dlq_deliveries_total family carries EXACTLY
// the four reason series, all at 0, and IncDLQDeliveries bumps only the
// named reason. Without the pre-initialization, /metrics (plain promhttp
// HandlerOpts — no OpenMetrics created timestamps) would first expose a
// reason series at its final value after a dead-letter, and Prometheus's
// increase() would compute last − first = 0: a single dead-letter per
// reason could never fire DLQDeadLetterRate.
func TestDLQDeliveriesCounter(t *testing.T) {
	t.Run("pre-initialized at exactly four zero series", func(t *testing.T) {
		c := New()
		mf := gatherFamily(t, c, "dlq_deliveries_total")
		if got := len(mf.GetMetric()); got != 4 {
			t.Fatalf("dlq_deliveries_total series count = %d, want 4 (got %v)", got, mf.GetMetric())
		}
		for _, reason := range []string{
			DLQReasonDecodeFailed, DLQReasonInvalidEnvelope,
			DLQReasonInvalidEvent, DLQReasonMaxRetries,
		} {
			if got := findMetric(t, mf, map[string]string{"reason": reason}).GetCounter().GetValue(); got != 0 {
				t.Fatalf("dlq_deliveries_total{reason=%q} = %v, want 0 immediately after New()", reason, got)
			}
		}
	})

	t.Run("IncDLQDeliveries bumps only the named reason", func(t *testing.T) {
		c := New()
		c.IncDLQDeliveries(DLQReasonDecodeFailed)
		c.IncDLQDeliveries(DLQReasonDecodeFailed)
		c.IncDLQDeliveries(DLQReasonMaxRetries)

		mf := gatherFamily(t, c, "dlq_deliveries_total")
		for reason, want := range map[string]float64{
			DLQReasonDecodeFailed:    2,
			DLQReasonInvalidEnvelope: 0,
			DLQReasonInvalidEvent:    0,
			DLQReasonMaxRetries:      1,
		} {
			if got := findMetric(t, mf, map[string]string{"reason": reason}).GetCounter().GetValue(); got != want {
				t.Fatalf("dlq_deliveries_total{reason=%q} = %v, want %v", reason, got, want)
			}
		}
	})
}

func TestServeMetricsEndpointAndGracefulStop(t *testing.T) {
	c := New()
	c.SetDLQDepth(3) // something observable in the exposition
	c.SetExportAge("tenant-a", time.Hour)
	c.ObserveHTTP("GET", "/healthz", 200, time.Millisecond)
	_, _ = c.GRPCUnaryInterceptor()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/svc/Method"},
		func(context.Context, any) (any, error) { return nil, nil })

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.ServeOn(ctx, lis, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	resp, err := http.Get("http://" + lis.Addr().String() + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		"dlq_depth_messages 3",
		"http_request_duration_seconds",
		"grpc_request_duration_seconds",
		"outbox_lag_seconds",
		"export_age_seconds",
		"go_goroutines", // the standard runtime collectors are registered too
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("/metrics body missing %q:\n%s", want, body)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeOn = %v, want nil on cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeOn did not return after cancel")
	}
}

// Failure path: a port already taken is a boot error, not a silent skip.
func TestServeListenFailure(t *testing.T) {
	c := New()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	if err := c.Serve(context.Background(), lis.Addr().String(),
		slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("Serve on a taken port = nil, want error")
	}
}
