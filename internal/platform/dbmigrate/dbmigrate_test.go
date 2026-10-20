package dbmigrate

import (
	"fmt"
	"os"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestReadMigrationsOrdersAndFilters(t *testing.T) {
	fsys := testFS(map[string]string{
		"0002_second.sql": "SELECT 2;",
		"0001_first.sql":  "SELECT 1;",
		"migrations.go":   "package migrations", // non-migration file ignored
		"notes.txt":       "not a migration",    // non-.sql file ignored
	})

	migrations, err := readMigrations(fsys)
	if err != nil {
		t.Fatalf("readMigrations: %v", err)
	}
	if len(migrations) != 2 {
		t.Fatalf("got %d migrations, want 2", len(migrations))
	}
	if migrations[0].name != "first" || migrations[1].name != "second" {
		t.Fatalf("order = [%s %s], want [first second]", migrations[0].name, migrations[1].name)
	}
}

func TestReadMigrationsRejectsDuplicateVersion(t *testing.T) {
	fsys := testFS(map[string]string{
		"0001_a.sql": "SELECT 1;",
		"0001_b.sql": "SELECT 2;",
	})
	if _, err := readMigrations(fsys); err == nil {
		t.Fatal("expected error for duplicate version, got nil")
	}
}

func TestReadMigrationsRejectsBadName(t *testing.T) {
	for _, name := range []string{"1_init.sql", "0001-Init.sql", "0001_.sql"} {
		if _, err := readMigrations(testFS(map[string]string{name: "SELECT 1;"})); err != nil {
			continue
		}
		t.Errorf("expected %q to be rejected", name)
	}
}

// --- integration tests (require PostgreSQL via TEST_DATABASE_URL) ---

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestApplyCreatesSchemaAndIsIdempotent(t *testing.T) {
	pool := integrationPool(t)
	schema := "dbmigrate_test_a"

	_, err := pool.Exec(t.Context(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", quoteIdent(schema)))
	if err != nil {
		t.Fatalf("drop schema: %v", err)
	}

	fsys := testFS(map[string]string{
		"0001_init.sql": fmt.Sprintf("CREATE TABLE %s.t (id int);", quoteIdent(schema)),
	})

	applied, err := Apply(t.Context(), pool, schema, fsys)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 1 || applied[0] != "init" {
		t.Fatalf("applied = %v, want [init]", applied)
	}

	applied, err = Apply(t.Context(), pool, schema, fsys)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Apply applied = %v, want none", applied)
	}

	var count int
	if err := pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", quoteIdent(schema))).Scan(&count); err != nil {
		t.Fatalf("query migrated table: %v", err)
	}
	if count != 0 {
		t.Fatalf("table t has %d rows, want 0", count)
	}
}

func TestApplyFailedMigrationLeavesNoRecord(t *testing.T) {
	pool := integrationPool(t)
	schema := "dbmigrate_test_b"

	_, err := pool.Exec(t.Context(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", quoteIdent(schema)))
	if err != nil {
		t.Fatalf("drop schema: %v", err)
	}

	fsys := testFS(map[string]string{
		"0001_ok.sql":     fmt.Sprintf("CREATE TABLE %s.before_failure (id int);", quoteIdent(schema)),
		"0002_broken.sql": "CREATE TABLE this_is_not_valid_sql;",
	})

	if _, err := Apply(t.Context(), pool, schema, fsys); err == nil {
		t.Fatal("expected Apply to fail on broken migration")
	}

	// The broken version must not be recorded, so a fixed rerun retries it.
	var recorded int
	if err := pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.schema_migrations", quoteIdent(schema))).Scan(&recorded); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("schema_migrations has %d rows, want 1 (only the migration before the failure)", recorded)
	}

	// A rerun with the failure fixed applies the repaired file.
	fixed := testFS(map[string]string{
		"0001_ok.sql":     fmt.Sprintf("CREATE TABLE %s.before_failure (id int);", quoteIdent(schema)),
		"0002_broken.sql": fmt.Sprintf("CREATE TABLE %s.fixed (id int);", quoteIdent(schema)),
	})
	applied, err := Apply(t.Context(), pool, schema, fixed)
	if err != nil {
		t.Fatalf("Apply after fix: %v", err)
	}
	if len(applied) != 1 || applied[0] != "broken" {
		t.Fatalf("applied = %v, want [broken]", applied)
	}
}
