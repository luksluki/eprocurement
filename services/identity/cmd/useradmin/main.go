// Command useradmin creates identity users with argon2id password hashes.
// It exists so the login flow can be exercised before the gateway and web
// shell issues land; it is a local/demo tool, not a production admin API.
//
//	go run ./services/identity/cmd/useradmin \
//	  -email admin@nusantara-retail.example -password 's3cret!' \
//	  -tenant-id 0b6b6c3f-... -role admin -role viewer -name "Admin"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/services/identity/auth"
	identityconfig "github.com/luksluki/eprocurement/services/identity/config"
)

func main() {
	email := flag.String("email", "", "login email (required)")
	password := flag.String("password", "", "login password (required)")
	tenantID := flag.String("tenant-id", "", "tenant uuid (required)")
	name := flag.String("name", "", "display name")
	roles := &multiFlagValue{}
	flag.Var(roles, "role", "role code, repeatable (admin/requester/approver/buyer/finance/viewer)")
	flag.Parse()

	if *email == "" || *password == "" || *tenantID == "" {
		fmt.Fprintln(os.Stderr, "useradmin: -email, -password, and -tenant-id are required")
		os.Exit(2)
	}

	cfg := identityconfig.Load()
	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	hash, err := auth.HashPassword(*password)
	if err != nil {
		fatal(err)
	}

	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID string
	err = tx.QueryRow(ctx, `
		INSERT INTO identity.users (tenant_id, email, display_name, password_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, lower(email)) DO UPDATE SET password_hash = EXCLUDED.password_hash
		RETURNING id`, *tenantID, *email, *name, hash).Scan(&userID)
	if err != nil {
		fatal(fmt.Errorf("upsert user: %w (is -tenant-id a uuid?)", err))
	}
	for _, role := range *roles {
		var roleID string
		if err := tx.QueryRow(ctx, `SELECT id FROM identity.roles WHERE code = $1`, role).Scan(&roleID); err != nil {
			fatal(fmt.Errorf("unknown role %q", role))
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.user_roles (user_id, role_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, userID, roleID); err != nil {
			fatal(fmt.Errorf("grant role %s: %w", role, err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		fatal(err)
	}
	fmt.Printf("user %s ready (email=%s roles=%v)\n", userID, *email, *roles)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "useradmin: %v\n", err)
	os.Exit(1)
}

type multiFlagValue []string

func (m *multiFlagValue) String() string { return "" }
func (m *multiFlagValue) Set(v string) error {
	*m = append(*m, v)
	return nil
}
