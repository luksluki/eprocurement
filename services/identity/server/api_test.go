package server

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// startIdentityServer boots the full gRPC wiring (interceptors + identity.v1
// API) against a real database, as TEST_DATABASE_URL provides.
func startIdentityServer(t *testing.T) (identityv1.IdentityClient, *pgxpool.Pool, func()) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	_, priv, err := jwt.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)) // keep test output clean
	svc := auth.New(db, log, priv, 1000)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(log, db, WithIdentityAPI(svc), WithHealthInterval(0)) // 0 interval disables ticks
	go func() { _ = srv.Serve(context.Background(), lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return identityv1.NewIdentityClient(conn), db, func() {
		_ = conn.Close()
		_ = lis.Close()
		db.Close()
	}
}

func TestGRPCEndToEndLoginRefreshLogout(t *testing.T) {
	client, db, cleanup := startIdentityServer(t)
	defer cleanup()

	email := "grpc@login.test"
	hash, err := auth.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := db.Exec(context.Background(), `
		INSERT INTO identity.users (tenant_id, email, display_name, password_hash)
		VALUES ($1, $2, 'gRPC Test', $3)
		ON CONFLICT (tenant_id, lower(email)) DO UPDATE SET password_hash = EXCLUDED.password_hash`,
		"33333333-3333-3333-3333-333333333333", email, hash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM identity.users WHERE email = $1`, email)
	})

	// Login.
	pair, err := client.Login(t.Context(), &identityv1.LoginRequest{Email: email, Password: "s3cret!"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if pair.GetTokenType() != "Bearer" || pair.GetAccessToken() == "" || pair.GetRefreshToken() == "" {
		t.Fatalf("unexpected pair: %+v", pair)
	}

	// Invalid credentials → Unauthenticated (gateway maps to 401), identical
	// message for unknown email and wrong password (no enumeration).
	_, errWrong := client.Login(t.Context(), &identityv1.LoginRequest{Email: email, Password: "nope"})
	_, errUnknown := client.Login(t.Context(), &identityv1.LoginRequest{Email: "ghost@login.test", Password: "nope"})
	if status.Code(errWrong) != codes.Unauthenticated || status.Code(errUnknown) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v / %v", status.Code(errWrong), status.Code(errUnknown))
	}
	if status.Convert(errWrong).Message() != status.Convert(errUnknown).Message() {
		t.Fatalf("error messages differ — enumeration leak: %q vs %q",
			status.Convert(errWrong).Message(), status.Convert(errUnknown).Message())
	}

	// Refresh rotation.
	rotated, err := client.Refresh(t.Context(), &identityv1.RefreshRequest{RefreshToken: pair.GetRefreshToken()})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := client.Refresh(t.Context(), &identityv1.RefreshRequest{RefreshToken: pair.GetRefreshToken()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("old refresh token err = %v, want Unauthenticated", err)
	}

	// Logout revokes the rotated token.
	if _, err := client.Logout(t.Context(), &identityv1.LogoutRequest{RefreshToken: rotated.GetRefreshToken()}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := client.Refresh(t.Context(), &identityv1.RefreshRequest{RefreshToken: rotated.GetRefreshToken()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("refresh after logout err = %v, want Unauthenticated", err)
	}
}
