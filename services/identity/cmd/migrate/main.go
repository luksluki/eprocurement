// Command migrate applies pending identity schema migrations and exits.
// Used by CI (and manual runs) to verify migrations apply cleanly; the service
// binary applies the same migrations on boot.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	identityconfig "github.com/luksluki/eprocurement/services/identity/config"
	"github.com/luksluki/eprocurement/services/identity/migrations"
)

func main() {
	cfg := identityconfig.Load()

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

	applied, err := dbmigrate.Apply(context.Background(), db, "identity", migrations.FS())
	if err != nil {
		fatal(fmt.Errorf("apply migrations: %w", err))
	}
	fmt.Printf("identity migrations up to date (applied %d)\n", len(applied))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
	os.Exit(1)
}
