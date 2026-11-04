package server

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	identityv1 "github.com/luksluki/eprocurement/gen/go/identity/v1"
	"github.com/luksluki/eprocurement/internal/platform/ratelimit"
)

// IdentityClient is the identity surface the gateway proxies. The generated
// gRPC client satisfies it; tests substitute a fake.
type IdentityClient interface {
	Login(ctx context.Context, in *identityv1.LoginRequest, opts ...grpc.CallOption) (*identityv1.LoginResponse, error)
	Refresh(ctx context.Context, in *identityv1.RefreshRequest, opts ...grpc.CallOption) (*identityv1.LoginResponse, error)
	Logout(ctx context.Context, in *identityv1.LogoutRequest, opts ...grpc.CallOption) (*identityv1.LogoutResponse, error)
}

// defaultLoginRatePerMin bounds the public login endpoint per client IP when
// no explicit budget is configured (PRD §17 requires throttling on auth).
const defaultLoginRatePerMin = 30

// loginLimiter throttles POST /api/v1/auth/login per client IP. All web
// traffic reaches the gateway through the Next.js BFF, so in the local demo
// the bucket is effectively per BFF instance; identity's 5/min per-email
// limiter remains as defense in depth (documented in .env.example).
type loginLimiter struct {
	limiter *ratelimit.Limiter
	perMin  int
}

func newLoginLimiter(perMin int) *loginLimiter {
	if perMin <= 0 {
		perMin = defaultLoginRatePerMin
	}
	return &loginLimiter{
		limiter: ratelimit.New(float64(perMin)/60.0, perMin),
		perMin:  perMin,
	}
}

// retryAfterSeconds is a conservative hint: the time until at least one token
// refills at the configured budget.
func (l *loginLimiter) retryAfterSeconds() int {
	return int(math.Ceil(60.0 / float64(l.perMin)))
}

// registerAuthRoutes wires the public auth API under /api/v1/auth — outside
// the bearerAuth group: login must accept anonymous traffic (ADR-0001 exposes
// identity's login through the gateway; verification stays on every other
// route).
func registerAuthRoutes(app *fiber.App, client IdentityClient, limiter *loginLimiter) {
	auth := app.Group("/api/v1/auth")
	auth.Post("/login", login(client, limiter))
	auth.Post("/refresh", refresh(client))
	auth.Post("/logout", logout(client))
}

type loginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshTokenBody struct {
	RefreshToken string `json:"refresh_token"`
}

// login serves POST /api/v1/auth/login: local body validation, a per-IP
// token bucket, then the identity Login RPC. Failure bodies stay generic —
// identity is the anti-enumeration authority (docs/schema/identity.md).
func login(client IdentityClient, limiter *loginLimiter) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var body loginBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		email := strings.TrimSpace(body.Email)
		if email == "" || body.Password == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email and password are required", "code": "body_invalid"})
		}
		if !limiter.limiter.Allow(c.IP()) {
			c.Set("Retry-After", itoa(limiter.retryAfterSeconds()))
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "too many login attempts, try again later", "code": "rate_limited"})
		}
		pair, err := client.Login(c.UserContext(), &identityv1.LoginRequest{Email: email, Password: body.Password})
		if err != nil {
			return authError(c, err, limiter)
		}
		return c.JSON(pairJSON(pair))
	}
}

// refresh serves POST /api/v1/auth/refresh: rotates the caller's refresh
// token via identity. The web BFF keeps tokens in httpOnly cookies and does
// not forward client refresh tokens directly.
func refresh(client IdentityClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var body refreshTokenBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		if strings.TrimSpace(body.RefreshToken) == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "refresh_token is required", "code": "body_invalid"})
		}
		pair, err := client.Refresh(c.UserContext(), &identityv1.RefreshRequest{RefreshToken: strings.TrimSpace(body.RefreshToken)})
		if err != nil {
			return authError(c, err, nil)
		}
		return c.JSON(pairJSON(pair))
	}
}

// logout serves POST /api/v1/auth/logout: revokes the refresh token.
// Identity logout is idempotent, so unknown tokens still return 204.
func logout(client IdentityClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var body refreshTokenBody
		if err := c.BodyParser(&body); err != nil || strings.TrimSpace(body.RefreshToken) == "" {
			// No token to revoke — logout is idempotent, succeed silently.
			return c.SendStatus(fiber.StatusNoContent)
		}
		if _, err := client.Logout(c.UserContext(), &identityv1.LogoutRequest{RefreshToken: strings.TrimSpace(body.RefreshToken)}); err != nil {
			return authError(c, err, nil)
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

func pairJSON(p *identityv1.LoginResponse) fiber.Map {
	return fiber.Map{
		"access_token":       p.GetAccessToken(),
		"access_expires_in":  p.GetAccessExpiresIn(),
		"refresh_token":      p.GetRefreshToken(),
		"refresh_expires_in": p.GetRefreshExpiresIn(),
		"token_type":         p.GetTokenType(),
	}
}

// authError maps identity's gRPC failures to HTTP. It deliberately does not
// share grpcHTTPError: the auth surface needs a ResourceExhausted → 429 arm
// (identity's per-email limiter) and a 502 "identity unavailable" default,
// and its bodies carry no machine-code table. Mapping: InvalidArgument→400
// (only reachable from local body validation in practice — identity itself
// never emits InvalidArgument today), Unauthenticated→401 (identity's generic
// anti-enumeration text passes through), ResourceExhausted→429 + Retry-After,
// everything else→502.
func authError(c *fiber.Ctx, err error, limiter *loginLimiter) error {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.InvalidArgument:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": st.Message(), "code": st.Message()})
	case codes.Unauthenticated:
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": st.Message(), "code": st.Message()})
	case codes.ResourceExhausted:
		retry := 60
		if limiter != nil {
			retry = limiter.retryAfterSeconds()
		}
		c.Set("Retry-After", itoa(retry))
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "too many login attempts, try again later", "code": "rate_limited"})
	default:
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "identity unavailable"})
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
