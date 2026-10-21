package auth

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

func testService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	_, priv, err := jwt.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return New(db, slog.Default(), NewKeyring(priv, nil), 1000), db // effectively unlimited rate for flow tests
}

// seedUser inserts a user with a real argon2id hash and the given role codes,
// returning the user id. Test fixtures span tenants, so they run under the
// RLS bypass.
func seedUser(t *testing.T, db *pgxpool.Pool, tenantID, email, password string, status string, roles ...string) string {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id string
	err = tenant.WithBypass(t.Context(), db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(t.Context(), `
			INSERT INTO identity.users (tenant_id, email, display_name, password_hash, status)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, lower(email)) DO UPDATE SET password_hash = EXCLUDED.password_hash, status = EXCLUDED.status
			RETURNING id`, tenantID, email, "Test User", hash, status).Scan(&id); err != nil {
			return err
		}
		for _, role := range roles {
			if _, err := tx.Exec(t.Context(), `
				INSERT INTO identity.user_roles (user_id, role_id)
				SELECT $1, id FROM identity.roles WHERE code = $2
				ON CONFLICT DO NOTHING`, id, role); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func cleanupUser(t *testing.T, db *pgxpool.Pool, email string) {
	t.Helper()
	_ = tenant.WithBypass(t.Context(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `DELETE FROM identity.users WHERE email = $1`, email)
		return err
	})
}

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

func TestLoginHappyPath(t *testing.T) {
	svc, db := testService(t)
	email := "happy@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active", "admin", "viewer")

	pair, err := svc.Login(t.Context(), email, "s3cret!", "k|1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("empty tokens in pair")
	}
	if pair.AccessExpiresIn != int64(AccessTTL.Seconds()) {
		t.Fatalf("access ttl = %d, want %d", pair.AccessExpiresIn, int64(AccessTTL.Seconds()))
	}
	if pair.RefreshExpiresIn != int64(RefreshTTL.Seconds()) {
		t.Fatalf("refresh ttl = %d, want %d", pair.RefreshExpiresIn, int64(RefreshTTL.Seconds()))
	}
}

// AC: invalid credentials return 401 without user enumeration — every
// failure mode must be the exact same error value.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, db := testService(t)
	email := "enum@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active", "viewer")

	cases := map[string]struct {
		email, password string
	}{
		"unknown email":     {"nobody@login.test", "s3cret!"},
		"wrong password":    {email, "wrong"},
		"disabled user":     {seedDisabled(t, db, email), "s3cret!"},
		"ambiguous tenant":  {seedAmbiguous(t, db), "s3cret!"},
		"empty credentials": {"", ""},
	}
	for name, tc := range cases {
		_, err := svc.Login(t.Context(), tc.email, tc.password, "k|"+name)
		if err == nil || err.Error() != ErrInvalidCredentials.Error() {
			t.Errorf("%s: err = %v, want the shared invalid-credentials error", name, err)
		}
	}
}

func seedDisabled(t *testing.T, db *pgxpool.Pool, base string) string {
	t.Helper()
	email := "disabled@" + base[len("happy@"):]
	seedUser(t, db, tenantA, email, "s3cret!", "disabled")
	return email
}

func seedAmbiguous(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	email := "shared@login.test"
	t.Cleanup(func() { cleanupUser(t, db, email) })
	seedUser(t, db, tenantA, email, "s3cret!", "active")
	seedUser(t, db, tenantB, email, "s3cret!", "active")
	return email
}

func TestLoginRateLimited(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, _ := pgxpool.New(context.Background(), dsn)
	t.Cleanup(db.Close)
	_, priv, _ := jwt.GenerateKey()
	svc := New(db, slog.Default(), NewKeyring(priv, nil), 2) // 2/min

	email := "rate@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active")

	for i := 0; i < 2; i++ {
		if _, err := svc.Login(t.Context(), email, "wrong", "same-key"); err == nil {
			t.Fatalf("attempt %d: expected credential error", i+1)
		}
	}
	if _, err := svc.Login(t.Context(), email, "s3cret!", "same-key"); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}

	// A different key (another address) is not affected.
	if _, err := svc.Login(t.Context(), email, "s3cret!", "other-key"); err != nil {
		t.Fatalf("other key should still work: %v", err)
	}
}

// AC: refresh rotation invalidates old token — and reuse revokes the family.
func TestRefreshRotationInvalidatesOldToken(t *testing.T) {
	svc, db := testService(t)
	email := "rotate@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active", "approver")

	first, err := svc.Login(t.Context(), email, "s3cret!", "k|1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	second, err := svc.Refresh(t.Context(), first.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// The rotated (old) token no longer works — and revokes the successor too.
	if _, err := svc.Refresh(t.Context(), first.RefreshToken); err != ErrInvalidRefreshToken {
		t.Fatalf("old token err = %v, want ErrInvalidRefreshToken", err)
	}
	if _, err := svc.Refresh(t.Context(), second.RefreshToken); err != ErrInvalidRefreshToken {
		t.Fatalf("reused family token err = %v, want ErrInvalidRefreshToken (family revoked)", err)
	}

	// A fresh login still works afterwards.
	if _, err := svc.Login(t.Context(), email, "s3cret!", "k|1b"); err != nil {
		t.Fatalf("fresh login after family revoke: %v", err)
	}
}

func TestRefreshChainRotation(t *testing.T) {
	svc, db := testService(t)
	email := "chain@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active")

	pair, err := svc.Login(t.Context(), email, "s3cret!", "k|1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	const chainLen = 5
	for i := 0; i < chainLen; i++ {
		pair, err = svc.Refresh(t.Context(), pair.RefreshToken)
		if err != nil {
			t.Fatalf("chain refresh %d: %v", i+1, err)
		}
	}
}

// AC: logout revokes refresh token.
func TestLogoutRevokesRefreshToken(t *testing.T) {
	svc, db := testService(t)
	email := "logout@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active")

	pair, err := svc.Login(t.Context(), email, "s3cret!", "k|1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.Logout(t.Context(), pair.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := svc.Refresh(t.Context(), pair.RefreshToken); err != ErrInvalidRefreshToken {
		t.Fatalf("refresh after logout err = %v, want ErrInvalidRefreshToken", err)
	}

	// Logout is idempotent and gives no hint about token existence.
	if err := svc.Logout(t.Context(), pair.RefreshToken); err != nil {
		t.Fatalf("second logout: %v", err)
	}
	if err := svc.Logout(t.Context(), "not-a-real-token"); err != nil {
		t.Fatalf("logout of unknown token should not error: %v", err)
	}
}

// The token's expiry is evaluated against an injected clock — no dependence
// on wall-time races, and no pgcrypto (the hash is computed in Go).
func TestRefreshExpiredTokenRejected(t *testing.T) {
	svc, db := testService(t)
	email := "expired@login.test"
	defer cleanupUser(t, db, email)
	userID := seedUser(t, db, tenantA, email, "s3cret!", "active")

	fixedNow := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedNow }
	sum := sha256.Sum256([]byte("expired-token"))
	err := tenant.WithBypass(t.Context(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `
			INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at, created_at)
			VALUES ($1, $2, $3, $4)`,
			userID, sum[:], fixedNow.Add(-time.Minute), fixedNow.Add(-8*24*time.Hour))
		return err
	})
	if err != nil {
		t.Fatalf("seed expired token: %v", err)
	}
	if _, err := svc.Refresh(t.Context(), "expired-token"); err != ErrInvalidRefreshToken {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

// A refresh token whose user was disabled after login must stop working.
func TestRefreshRejectedWhenUserDisabled(t *testing.T) {
	svc, db := testService(t)
	email := "disabled-refresh@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active")

	pair, err := svc.Login(t.Context(), email, "s3cret!", "k|1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := tenant.WithBypass(t.Context(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE identity.users SET status = 'disabled' WHERE email = $1`, email)
		return err
	}); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	if _, err := svc.Refresh(t.Context(), pair.RefreshToken); err != ErrInvalidRefreshToken {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

// A corrupted stored hash must produce the same generic credential error as
// any other login failure — never an internal error or a leak.
func TestLoginMalformedStoredHashFailsClosed(t *testing.T) {
	svc, db := testService(t)
	email := "corrupt-hash@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active")

	err := tenant.WithBypass(t.Context(), db, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE identity.users SET password_hash = 'not-a-phc-hash' WHERE email = $1`, email)
		return err
	})
	if err != nil {
		t.Fatalf("corrupt hash: %v", err)
	}

	_, err = svc.Login(t.Context(), email, "s3cret!", "k|1")
	if err == nil || err.Error() != ErrInvalidCredentials.Error() {
		t.Fatalf("err = %v, want the shared invalid-credentials error", err)
	}
}

func TestAccessJWTCarriesTenantAndRoles(t *testing.T) {
	svc, db := testService(t)
	email := "claims@login.test"
	defer cleanupUser(t, db, email)
	seedUser(t, db, tenantA, email, "s3cret!", "active", "buyer", "finance")

	pair, err := svc.Login(t.Context(), email, "s3cret!", "k|1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	var claims AccessClaims
	if err := jwt.Parse(svc.PublicKey(), pair.AccessToken, &claims, time.Now()); err != nil {
		t.Fatalf("parse access token: %v", err)
	}
	if claims.TenantID != tenantA {
		t.Fatalf("tid = %s, want %s", claims.TenantID, tenantA)
	}
	if len(claims.Roles) != 2 || claims.Roles[0] != "buyer" || claims.Roles[1] != "finance" {
		t.Fatalf("roles = %v, want [buyer finance]", claims.Roles)
	}
	if claims.Iss != "identity.eprocurement" || claims.Sub == "" || claims.JTI == "" {
		t.Fatalf("claims missing iss/sub/jti: %+v", claims)
	}
	if claims.EXP-time.Now().Unix() > int64(AccessTTL.Seconds()) {
		t.Fatalf("exp is further out than the 15-minute access TTL: %d", claims.EXP-time.Now().Unix())
	}
}
