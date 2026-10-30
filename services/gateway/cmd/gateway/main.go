// Command gateway runs the REST edge/BFF: it terminates HTTP, verifies
// Bearer access tokens against the identity service's JWKS (cached), and
// serves the public key set for other verifiers. It also hosts the admin
// DLQ inspection/replay routes (#48) backed by its own small AMQP client —
// the gateway's one direct broker dependency (maintainer option A,
// 2026-10-28), dialed per request so a down broker costs only those
// requests (503), never boot.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	gatewayconfig "github.com/luksluki/eprocurement/services/gateway/config"
	"github.com/luksluki/eprocurement/services/gateway/dlq"
	"github.com/luksluki/eprocurement/services/gateway/server"
	"github.com/luksluki/eprocurement/services/gateway/token"
	"github.com/luksluki/eprocurement/services/worker/topology"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := gatewayconfig.Load()
	log := logger.New("gateway", cfg.Env, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := grpc.NewClient(cfg.IdentityAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial identity %s: %w", cfg.IdentityAddr, err)
	}
	defer func() { _ = conn.Close() }()

	verifier := token.NewVerifier(
		token.NewGRPCFetcher(identityv1.NewIdentityClient(conn)),
		cfg.JWKSCacheTTL,
	)

	procurementConn, err := grpc.NewClient(cfg.ProcurementAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial procurement %s: %w", cfg.ProcurementAddr, err)
	}
	defer func() { _ = procurementConn.Close() }()

	documentsConn, err := grpc.NewClient(cfg.DocumentsAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// #44: the audit CSV export rides one unary response (bounded by
		// documents at the newest 10 000 rows, metadata columns only — a few
		// MB at worst). The gRPC default receive limit is 4 MiB, so the dial
		// raises it explicitly to 16 MiB rather than relying on the default.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		return fmt.Errorf("dial documents %s: %w", cfg.DocumentsAddr, err)
	}
	defer func() { _ = documentsConn.Close() }()

	app := server.New(log, verifier,
		server.WithIdentity(identityv1.NewIdentityClient(conn), cfg.LoginRatePerMin),
		server.WithProcurement(procurementv1.NewProcurementClient(procurementConn)),
		server.WithDocuments(documentsv1.NewDocumentsClient(documentsConn)),
		// #44: whose X-Forwarded-For the gateway honors for c.IP() (the
		// audit actor_ip source). Empty = trust no proxy: every forwarded
		// header is ignored and the socket peer is recorded.
		server.WithTrustedProxies(cfg.TrustedProxies),
		// The admin DLQ surface (#48): the gateway's own small AMQP client
		// over procurement.dlq (maintainer option A, 2026-10-28). It dials
		// per request; a broker that is down fails those requests with 503
		// and nothing else — no boot-time dependency.
		server.WithDLQ(dlq.New(cfg.RabbitMQURL, topology.Default)))
	log.Info("starting gateway", "port", cfg.Port,
		"identity_addr", cfg.IdentityAddr, "procurement_addr", cfg.ProcurementAddr,
		"documents_addr", cfg.DocumentsAddr, "login_rate_per_min", cfg.LoginRatePerMin,
		"trusted_proxies", len(cfg.TrustedProxies))

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = app.ShutdownWithContext(shutdownCtx)
	}()

	if err := app.Listen(":" + cfg.Port); err != nil {
		// Listen returns nil on graceful shutdown.
		select {
		case <-ctx.Done():
			log.Info("gateway stopped")
			return nil
		default:
			return fmt.Errorf("listen %s: %w", cfg.Port, err)
		}
	}
	return nil
}
