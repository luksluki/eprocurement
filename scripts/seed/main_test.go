package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestNewUUIDDeterministic(t *testing.T) {
	a := newUUID(rand.New(rand.NewSource(7)))
	b := newUUID(rand.New(rand.NewSource(7)))
	if a != b {
		t.Fatalf("expected deterministic UUID, got %q and %q", a, b)
	}
	if len(a) != 36 {
		t.Fatalf("expected canonical UUID length 36, got %d", len(a))
	}
}

func TestLoadDotEnv(t *testing.T) {
	key := "SEED_TEST_KEY_FOR_TESTS"
	_ = os.Unsetenv(key)

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("# comment\n"+key+"=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loadDotEnv(path)
	if got := os.Getenv(key); got != "from-file" {
		t.Fatalf("expected from-file, got %q", got)
	}

	if err := os.Setenv(key, "from-env"); err != nil {
		t.Fatal(err)
	}
	loadDotEnv(path)
	if got := os.Getenv(key); got != "from-env" {
		t.Fatalf("environment must win, got %q", got)
	}
	_ = os.Unsetenv(key)
}
