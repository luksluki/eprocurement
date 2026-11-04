// Package dbmigrate applies embedded SQL migrations to a PostgreSQL schema.
//
// Migrations are *.sql files in the provided filesystem, named
// <version>_<name>.sql (e.g. 0001_init.sql) and applied in ascending version
// order. Each file runs inside a transaction together with the insert into
// <schema>.schema_migrations, so a failed file leaves no partial state and is
// retried on the next run. Files whose version is already recorded are skipped,
// making Apply idempotent.
//
// Pool requirement: a migration file can contain multiple statements, so the
// pool must be created with pgx.QueryExecModeSimpleProtocol (see the cmd
// wiring in services/identity).
package dbmigrate

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type migration struct {
	version int64
	name    string
	sql     string
}

// Apply runs all pending migrations from fsys against the given schema and
// returns the names of the migrations it applied.
func Apply(ctx context.Context, db *pgxpool.Pool, schema string, fsys fs.FS) ([]string, error) {
	if err := db.Ping(ctx); err != nil {
		return nil, fmt.Errorf("dbmigrate: ping: %w", err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdent(schema))); err != nil {
		return nil, fmt.Errorf("dbmigrate: create schema %s: %w", schema, err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s.schema_migrations (
			version bigint PRIMARY KEY,
			name text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`, quoteIdent(schema))); err != nil {
		return nil, fmt.Errorf("dbmigrate: ensure schema_migrations: %w", err)
	}

	migrations, err := readMigrations(fsys)
	if err != nil {
		return nil, err
	}

	var applied []string
	for _, m := range migrations {
		var recorded bool
		err := db.QueryRow(ctx,
			fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s.schema_migrations WHERE version = $1)", quoteIdent(schema)),
			m.version,
		).Scan(&recorded)
		if err != nil {
			return nil, fmt.Errorf("dbmigrate: check version %d: %w", m.version, err)
		}
		if recorded {
			continue
		}
		if err := applyOne(ctx, db, schema, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.name)
	}
	return applied, nil
}

func applyOne(ctx context.Context, db *pgxpool.Pool, schema string, m migration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("dbmigrate: begin %04d_%s: %w", m.version, m.name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("dbmigrate: apply %04d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s.schema_migrations (version, name) VALUES ($1, $2)", quoteIdent(schema)),
		m.version, m.name,
	); err != nil {
		return fmt.Errorf("dbmigrate: record %04d_%s: %w", m.version, m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("dbmigrate: commit %04d_%s: %w", m.version, m.name, err)
	}
	return nil
}

var namePattern = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// readMigrations loads and validates migration files in version order.
func readMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: read dir: %w", err)
	}

	var migrations []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := namePattern.FindStringSubmatch(e.Name())
		if match == nil {
			// Non-SQL files (embed.go, READMEs) are ignored; a .sql file that
			// does not match the naming scheme is an error, otherwise a
			// migration would be silently skipped.
			if strings.HasSuffix(e.Name(), ".sql") {
				return nil, fmt.Errorf("dbmigrate: %s does not match <4-digit-version>_<name>.sql", e.Name())
			}
			continue
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("dbmigrate: parse version %s: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("dbmigrate: read %s: %w", e.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: match[2], sql: string(body)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for i := 1; i < len(migrations); i++ {
		if migrations[i].version == migrations[i-1].version {
			return nil, fmt.Errorf("dbmigrate: duplicate version %04d (%s, %s)",
				migrations[i].version, migrations[i-1].name, migrations[i].name)
		}
	}
	return migrations, nil
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
