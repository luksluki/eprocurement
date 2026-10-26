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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	procurementconfig "github.com/luksluki/eprocurement/services/procurement/config"
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
	log.Info("starting procurement service", "port", cfg.Port)

	// Migrations run on boot with a simple-protocol pool (multi-statement SQL).
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

	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Port, err)
	}
	srv := server.New(log, db, vendor.NewStore(db), requisition.NewStore(db), approval.NewStore(db), purchaseorder.NewStore(db))
	return srv.Serve(ctx, lis)
}
