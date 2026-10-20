package server

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// GetJwks needs no database — the key set lives in memory. This test drives
// the gRPC adapter directly with a nil-DB service.
func TestGetJwksServesKeyring(t *testing.T) {
	currentPub, currentPriv, _ := jwt.GenerateKey()
	_, previousPriv, _ := jwt.GenerateKey()

	svc := auth.New(nil, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		auth.NewKeyring(currentPriv, previousPriv), 5)
	api := newAPI(slog.Default(), svc)

	resp, err := api.GetJwks(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetJwks: %v", err)
	}

	if len(resp.GetKeys()) != 2 {
		t.Fatalf("keys = %d, want 2 (current + previous during rotation)", len(resp.GetKeys()))
	}
	cur := resp.GetKeys()[0]
	if !cur.GetCurrent() || cur.GetKid() != jwt.Kid(currentPub) || len(cur.GetEd25519()) != 32 {
		t.Fatalf("current key = %+v", cur)
	}
	if resp.GetKeys()[1].GetCurrent() {
		t.Fatal("previous key must not be marked current")
	}

	// A keyring without a previous key serves exactly one key.
	solo := auth.New(nil, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		auth.NewKeyring(currentPriv, nil), 5)
	resp, err = newAPI(slog.Default(), solo).GetJwks(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetJwks solo: %v", err)
	}
	if len(resp.GetKeys()) != 1 || !resp.GetKeys()[0].GetCurrent() {
		t.Fatalf("solo keys = %+v", resp.GetKeys())
	}
}
