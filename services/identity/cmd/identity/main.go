// Command identity runs the identity service: it applies pending schema
// migrations, then serves gRPC (health + reflection) until SIGINT/SIGTERM.
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
	identityconfig "github.com/luksluki/eprocurement/services/identity/config"
	"github.com/luksluki/eprocurement/services/identity/migrations"
	"github.com/luksluki/eprocurement/services/identity/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "identity: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := identityconfig.Load()
	log := logger.New("identity", cfg.Env, cfg.LogLevel)
	log.Info("starting identity service", "port", cfg.Port)

	// Migrations run on boot with a simple-protocol pool (multi-statement SQL);
	// the runtime pool below uses the default prepared-statement mode.
	migCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		return fmt.Errorf("open migration pool: %w", err)
	}
	applied, err := dbmigrate.Apply(context.Background(), migPool, "identity", migrations.FS())
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
	srv := server.New(log, db)
	return srv.Serve(ctx, lis)
}
