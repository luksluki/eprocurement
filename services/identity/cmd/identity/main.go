// Command identity runs the identity service: it applies pending schema
// migrations, then serves gRPC (identity.v1 login/refresh/logout + health +
// reflection) until SIGINT/SIGTERM.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	platformjwt "github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/identity/auth"
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

	priv, err := signingKey(log, "IDENTITY_JWT_KEY", cfg.JWTKeySeed)
	if err != nil {
		return err
	}
	var previous ed25519.PrivateKey
	if cfg.JWTKeyPrevSeed != "" {
		if previous, err = signingKey(log, "IDENTITY_JWT_KEY_PREVIOUS", cfg.JWTKeyPrevSeed); err != nil {
			return err
		}
	}
	authSvc := auth.New(db, log, auth.NewKeyring(priv, previous), cfg.LoginRatePerMin)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Port, err)
	}
	srv := server.New(log, db, server.WithIdentityAPI(authSvc))
	return srv.Serve(ctx, lis)
}

// signingKey resolves one Ed25519 signing key from a base64 seed. An empty
// value for the current key yields an ephemeral boot key (access tokens then
// do not survive restarts — local development only).
func signingKey(log *slog.Logger, envName, seedB64 string) (ed25519.PrivateKey, error) {
	if seedB64 == "" {
		_, priv, err := platformjwt.GenerateKey()
		if err != nil {
			return nil, fmt.Errorf("generate ephemeral signing key: %w", err)
		}
		log.Warn("no " + envName + " set; using an ephemeral signing key (tokens do not survive restarts)")
		return priv, nil
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", envName, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s must decode to %d bytes, got %d", envName, ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
