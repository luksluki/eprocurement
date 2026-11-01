// Command procurement runs the procurement service: it applies pending
// schema migrations, then serves gRPC (procurement.v1 + health + reflection)
// until SIGINT/SIGTERM.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/grpcserver"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/internal/platform/metrics"
	"github.com/luksluki/eprocurement/internal/platform/telemetry"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	procurementconfig "github.com/luksluki/eprocurement/services/procurement/config"
	"github.com/luksluki/eprocurement/services/procurement/dashboard"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/match"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/server"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "procurement: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := procurementconfig.Load()
	log := logger.New("procurement", cfg.Env, cfg.LogLevel)
	log.Info("starting procurement service", "port", cfg.Port, "metrics_port", cfg.MetricsPort)

	// #54: the OTel SDK — set up before serving; every request span (gRPC
	// server interceptor in grpcserver.New) exports from here. Empty
	// endpoint = SDK active, nothing exported (local/CI).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tp, err := telemetry.Setup(ctx, "procurement", cfg.Env, cfg.OTLPEndpoint)
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}
	defer telemetry.Shutdown(context.Background(), tp, 5*time.Second)

	// Migrations run on boot with a simple-protocol pool (multi-statement
	// SQL); deliberately WITHOUT the #54 query tracer — boot-time, once per
	// deploy, before any request exists (see docs/dev-notes/054).
	migCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		return fmt.Errorf("open migration pool: %w", err)
	}
	applied, err := dbmigrate.Apply(context.Background(), migPool, "procurement", migrations.FS())
	migPool.Close()
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	if len(applied) > 0 {
		log.Info("migrations applied", "count", len(applied), "names", applied)
	}

	// The RUNTIME pool carries the #54 pgx query tracer (a span per SQL
	// statement under the request's trace; no parameters recorded).
	poolCfg, err := telemetry.PGXPoolConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	db, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer db.Close()

	// #55: gRPC latency histogram + the /metrics server below.
	mc := metrics.New()
	metricsDone := make(chan error, 1)
	go func() {
		err := mc.Serve(ctx, ":"+cfg.MetricsPort, log)
		if err != nil {
			stop() // unblock the gRPC Serve; the error is returned below
		}
		metricsDone <- err
	}()

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Port, err)
	}
	srv := server.New(log, db, vendor.NewStore(db), requisition.NewStore(db), approval.NewStore(db), purchaseorder.NewStore(db), goodsreceipt.NewStore(db), invoice.NewStore(db), match.NewStore(db), dashboard.NewStore(db),
		grpcserver.WithMetrics(mc))
	serveErr := srv.Serve(ctx, lis)
	stop()
	metricsErr := <-metricsDone
	if serveErr != nil {
		return serveErr
	}
	return metricsErr
}
