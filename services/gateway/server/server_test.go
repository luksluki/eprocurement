package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/services/gateway/token"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// staticFetcher feeds the verifier a fixed key set (tests mint their own
// tokens with the same keys).
type staticFetcher struct {
	keys []token.Key
}

func (f *staticFetcher) FetchKeys(ctx context.Context) ([]token.Key, error) {
	return f.keys, nil
}

func newTestApp(t *testing.T) (*fiber.App, *token.Verifier, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, _ := jwt.GenerateKey()
	kid := jwt.Kid(pub)

	verifier := token.NewVerifier(&staticFetcher{keys: []token.Key{{Kid: kid, Ed25519: pub, Current: true}}}, time.Hour)
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)) // keep test output clean
	app := New(log, verifier)

	t.Cleanup(func() { _ = app.Shutdown() })
	return app, verifier, priv, kid
}

func do(t *testing.T, app *fiber.App, method, path, authHeader string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp, body
}

func TestHealthz(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	resp, body := do(t, app, "GET", "/healthz", "")
	if resp.StatusCode != fiber.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz = %d %v", resp.StatusCode, body)
	}
}

func TestJwksShape(t *testing.T) {
	app, _, _, kid := newTestApp(t)
	resp, body := do(t, app, "GET", "/.well-known/jwks.json", "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("jwks status = %d", resp.StatusCode)
	}
	keys, _ := body["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("jwks keys = %v", body)
	}
	key := keys[0].(map[string]any)
	if key["kty"] != "OKP" || key["crv"] != "Ed25519" || key["alg"] != "EdDSA" || key["kid"] != kid {
		t.Fatalf("jwks key = %v", key)
	}
	// x must be the base64url encoding of the raw 32-byte public key.
	x, _ := key["x"].(string)
	raw, err := base64.RawURLEncoding.DecodeString(x)
	if err != nil || len(raw) != 32 {
		t.Fatalf("jwks x invalid: %q (%v)", x, err)
	}
}

func TestMeRequiresValidToken(t *testing.T) {
	app, _, _, _ := newTestApp(t)

	for name, header := range map[string]string{
		"no token":     "",
		"garbage":      "Bearer garbage",
		"wrong scheme": "Basic abc",
	} {
		resp, _ := do(t, app, "GET", "/api/v1/me", header)
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
}

// AC: gateway verifies tokens using JWKS — mint an identity-claims token and
// verify it end-to-end through the fiber stack.
func TestMeVerifiesToken(t *testing.T) {
	app, _, priv, kid := newTestApp(t)

	now := time.Now()
	claims := auth.AccessClaims{
		Iss: "identity.eprocurement", Sub: "user-1", TenantID: "tenant-1",
		Roles: []string{"admin", "viewer"}, JTI: "jti-1",
		IAT: now.Unix(), EXP: now.Add(auth.AccessTTL).Unix(),
	}
	access, err := jwt.Sign(priv, kid, claims, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	resp, body := do(t, app, "GET", "/api/v1/me", "Bearer "+access)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("me status = %d, body = %v", resp.StatusCode, body)
	}
	if body["user_id"] != "user-1" || body["tenant_id"] != "tenant-1" {
		t.Fatalf("me body = %v", body)
	}
	roles, _ := body["roles"].([]any)
	if len(roles) != 2 || roles[0] != "admin" {
		t.Fatalf("me roles = %v", body["roles"])
	}

	// Expired token → 401.
	expired, _ := jwt.Sign(priv, kid, auth.AccessClaims{Sub: "u", EXP: now.Add(-time.Minute).Unix()}, now)
	resp, _ = do(t, app, "GET", "/api/v1/me", "Bearer "+expired)
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401", resp.StatusCode)
	}
}
