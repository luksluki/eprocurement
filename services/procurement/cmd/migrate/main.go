// Command migrate applies pending procurement schema migrations and exits.
// Used by CI (and manual runs); the service binary applies the same
// migrations on boot.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	procurementconfig "github.com/luksluki/eprocurement/services/procurement/config"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
)

func main() {
	cfg := procurementconfig.Load()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		fatal(fmt.Errorf("parse database url: %w", err))
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	db, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		fatal(fmt.Errorf("open pool: %w", err))
	}
	defer db.Close()

	applied, err := dbmigrate.Apply(context.Background(), db, "procurement", migrations.FS())
	if err != nil {
		fatal(fmt.Errorf("apply migrations: %w", err))
	}
	fmt.Printf("procurement migrations up to date (applied %d)\n", len(applied))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
	os.Exit(1)
}
