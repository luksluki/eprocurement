package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// fakeIdentityClient answers the three auth RPCs with canned pairs or errors
// and counts calls so tests can prove the limiter short-circuits identity.
type fakeIdentityClient struct {
	loginErr   error
	refreshErr error
	logoutErr  error
	pair       *identityv1.LoginResponse

	loginCalls   int
	refreshCalls int
	logoutCalls  int

	lastLoginReq   *identityv1.LoginRequest
	lastRefreshReq *identityv1.RefreshRequest
	lastLogoutReq  *identityv1.LogoutRequest
}

func (f *fakeIdentityClient) Login(ctx context.Context, in *identityv1.LoginRequest, opts ...grpc.CallOption) (*identityv1.LoginResponse, error) {
	f.loginCalls++
	f.lastLoginReq = in
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	return f.pair, nil
}

func (f *fakeIdentityClient) Refresh(ctx context.Context, in *identityv1.RefreshRequest, opts ...grpc.CallOption) (*identityv1.LoginResponse, error) {
	f.refreshCalls++
	f.lastRefreshReq = in
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.pair, nil
}

func (f *fakeIdentityClient) Logout(ctx context.Context, in *identityv1.LogoutRequest, opts ...grpc.CallOption) (*identityv1.LogoutResponse, error) {
	f.logoutCalls++
	f.lastLogoutReq = in
	if f.logoutErr != nil {
		return nil, f.logoutErr
	}
	return &identityv1.LogoutResponse{}, nil
}

func testPair() *identityv1.LoginResponse {
	return &identityv1.LoginResponse{
		AccessToken: "at-1", AccessExpiresIn: 900,
		RefreshToken: "rt-1", RefreshExpiresIn: 604800, TokenType: "Bearer",
	}
}

func postJSON(t *testing.T, app interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, path, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test %s: %v", path, err)
	}
	return resp
}

// TestAuthRoutes exercises login/refresh/logout end-to-end through the fiber
// stack: happy paths, local body validation, and identity failure mapping.
func TestAuthRoutes(t *testing.T) {
	tests := []struct {
		name       string
		setup      *fakeIdentityClient
		path       string
		body       string
		wantStatus int
		wantBody   string // substring of the JSON body, "" skips
	}{
		{
			name:       "login happy path",
			setup:      &fakeIdentityClient{pair: testPair()},
			path:       "/api/v1/auth/login",
			body:       `{"email":"admin@example.test","password":"s3cret"}`,
			wantStatus: http.StatusOK,
			wantBody:   `"access_token":"at-1"`,
		},
		{
			name:       "login malformed JSON",
			setup:      &fakeIdentityClient{pair: testPair()},
			path:       "/api/v1/auth/login",
			body:       `{"email":`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid JSON body",
		},
		{
			name:       "login missing password",
			setup:      &fakeIdentityClient{pair: testPair()},
			path:       "/api/v1/auth/login",
			body:       `{"email":"admin@example.test"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "email and password are required",
		},
		{
			name:       "login wrong credentials (identity Unauthenticated)",
			setup:      &fakeIdentityClient{loginErr: status.Error(codes.Unauthenticated, "invalid email or password")},
			path:       "/api/v1/auth/login",
			body:       `{"email":"admin@example.test","password":"wrong"}`,
			wantStatus: http.StatusUnauthorized,
			wantBody:   "invalid email or password",
		},
		{
			name:       "refresh happy path returns rotated pair",
			setup:      &fakeIdentityClient{pair: testPair()},
			path:       "/api/v1/auth/refresh",
			body:       `{"refresh_token":"rt-old"}`,
			wantStatus: http.StatusOK,
			wantBody:   `"refresh_token":"rt-1"`,
		},
		{
			name:       "refresh missing token",
			setup:      &fakeIdentityClient{pair: testPair()},
			path:       "/api/v1/auth/refresh",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "refresh_token is required",
		},
		{
			name:       "refresh rotated token (identity rejects)",
			setup:      &fakeIdentityClient{refreshErr: status.Error(codes.Unauthenticated, "invalid refresh token")},
			path:       "/api/v1/auth/refresh",
			body:       `{"refresh_token":"rt-reused"}`,
			wantStatus: http.StatusUnauthorized,
			wantBody:   "invalid refresh token",
		},
		{
			name:       "logout happy path",
			setup:      &fakeIdentityClient{},
			path:       "/api/v1/auth/logout",
			body:       `{"refresh_token":"rt-1"}`,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "logout without body stays idempotent",
			setup:      &fakeIdentityClient{},
			path:       "/api/v1/auth/logout",
			body:       ``,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "identity unreachable maps to 502",
			setup:      &fakeIdentityClient{loginErr: status.Error(codes.Unavailable, "connection refused")},
			path:       "/api/v1/auth/login",
			body:       `{"email":"a@b.test","password":"x"}`,
			wantStatus: http.StatusBadGateway,
			wantBody:   "identity unavailable",
		},
		{
			name:       "identity per-email limiter (ResourceExhausted) maps to 429",
			setup:      &fakeIdentityClient{loginErr: status.Error(codes.ResourceExhausted, "too many login attempts, try again later")},
			path:       "/api/v1/auth/login",
			body:       `{"email":"a@b.test","password":"x"}`,
			wantStatus: http.StatusTooManyRequests,
			wantBody:   "too many login attempts",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			registerAuthRoutes(app, tt.setup, newLoginLimiter(30))

			resp := postJSON(t, app, tt.path, tt.body)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantBody != "" && resp.StatusCode != http.StatusNoContent {
				buf := new(bytes.Buffer)
				_, _ = buf.ReadFrom(resp.Body)
				if !strings.Contains(buf.String(), tt.wantBody) {
					t.Fatalf("body = %q, want substring %q", buf.String(), tt.wantBody)
				}
			}
		})
	}
}

// TestAuthLoginRateLimited proves the per-IP token bucket trips before the
// identity call: with a 2/min budget the third burst attempt gets 429 +
// Retry-After, and identity saw exactly two logins. Refresh is not limited.
func TestAuthLoginRateLimited(t *testing.T) {
	fake := &fakeIdentityClient{pair: testPair()}
	app, _, _, _ := newTestApp(t)
	registerAuthRoutes(app, fake, newLoginLimiter(2))

	for i := 0; i < 2; i++ {
		resp := postJSON(t, app, "/api/v1/auth/login", `{"email":"a@b.test","password":"x"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}

	resp := postJSON(t, app, "/api/v1/auth/login", `{"email":"a@b.test","password":"x"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third attempt status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" { // ceil(60/2)
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if fake.loginCalls != 2 {
		t.Fatalf("identity login calls = %d, want 2 (limiter must short-circuit)", fake.loginCalls)
	}

	// The limiter is login-only: refresh on the same IP still goes through.
	rresp := postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"rt-x"}`)
	if rresp.StatusCode != http.StatusOK || fake.refreshCalls != 1 {
		t.Fatalf("refresh after login throttle: status = %d, calls = %d, want 200/1", rresp.StatusCode, fake.refreshCalls)
	}
}

// TestAuthRoutesPublicButMeProtected pins the routing split: auth routes
// answer anonymous requests (no Authorization header) while /api/v1/me keeps
// requiring a Bearer token.
func TestAuthRoutesPublicButMeProtected(t *testing.T) {
	fake := &fakeIdentityClient{pair: testPair()}
	app, _, _, _ := newTestApp(t)
	registerAuthRoutes(app, fake, newLoginLimiter(30))

	resp := postJSON(t, app, "/api/v1/auth/login", `{"email":"a@b.test","password":"x"}`)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("anonymous login was rejected with 401 — auth routes must be public")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous login status = %d, want 200", resp.StatusCode)
	}

	// /me on the same app still demands a token.
	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	mresp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test me: %v", err)
	}
	if mresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous /me status = %d, want 401", mresp.StatusCode)
	}
}

// TestAuthWiringThroughNew proves the WithIdentity option registers the
// public routes in the real construction path used by cmd/gateway.
func TestAuthWiringThroughNew(t *testing.T) {
	fake := &fakeIdentityClient{pair: testPair()}
	pub, _, _ := jwt.GenerateKey()
	kid := jwt.Kid(pub)
	verifier := token.NewVerifier(&staticFetcher{keys: []token.Key{{Kid: kid, Ed25519: pub, Current: true}}}, time.Hour)
	app := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), verifier, WithIdentity(fake, 30))
	t.Cleanup(func() { _ = app.Shutdown() })

	resp := postJSON(t, app, "/api/v1/auth/login", `{"email":"a@b.test","password":"x"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login through New status = %d, want 200", resp.StatusCode)
	}
}
