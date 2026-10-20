// Command grpc-health-probe checks a gRPC health endpoint and exits non-zero
// when the target is not serving. It is the healthcheck used by compose and
// works against the standard grpc.health.v1 protocol, so it serves any service
// in this repo without per-service health handlers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	addr := flag.String("addr", "localhost:8081", "gRPC host:port to probe")
	service := flag.String("service", "", "health service name (empty = overall server health)")
	timeout := flag.Duration("timeout", 3*time.Second, "probe timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal(*addr, fmt.Errorf("dial: %w", err))
	}
	defer func() { _ = conn.Close() }()

	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: *service})
	if err != nil {
		fatal(*addr, err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		fmt.Fprintf(os.Stderr, "grpc-health-probe: %s service=%q status=%s\n", *addr, *service, resp.GetStatus())
		os.Exit(1)
	}
	fmt.Printf("grpc-health-probe: %s service=%q SERVING\n", *addr, *service)
}

func fatal(addr string, err error) {
	fmt.Fprintf(os.Stderr, "grpc-health-probe: %s: %v\n", addr, err)
	os.Exit(1)
}
