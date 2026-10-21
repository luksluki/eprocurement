// Command gateway runs the REST edge/BFF: it terminates HTTP, verifies
// Bearer access tokens against the identity service's JWKS (cached), and
// serves the public key set for other verifiers. Business routes arrive with
// the later service issues.
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

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	gatewayconfig "github.com/luksluki/eprocurement/services/gateway/config"
	"github.com/luksluki/eprocurement/services/gateway/server"
	"github.com/luksluki/eprocurement/services/gateway/token"
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

	app := server.New(log, verifier,
		server.WithProcurement(procurementv1.NewProcurementClient(procurementConn)))
	log.Info("starting gateway", "port", cfg.Port,
		"identity_addr", cfg.IdentityAddr, "procurement_addr", cfg.ProcurementAddr)

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
