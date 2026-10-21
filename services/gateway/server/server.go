// Package server wires the gateway's GoFiber app: health, the public JWKS
// endpoint, Bearer-token verification against the identity JWKS cache, and a
// minimal authenticated endpoint (/api/v1/me) that demonstrates verification.
// Business REST routes arrive with the later service issues.
package server

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/gateway/token"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// Option adds a proxied service to the gateway.
type Option func(*gatewayOptions)

type gatewayOptions struct {
	procurement VendorClient
}

// WithProcurement proxies the procurement vendor API under /api/v1/vendors.
func WithProcurement(client VendorClient) Option {
	return func(o *gatewayOptions) { o.procurement = client }
}

// New builds the fiber app.
func New(log *slog.Logger, verifier *token.Verifier, opts ...Option) *fiber.App {
	var o gatewayOptions
	for _, opt := range opts {
		opt(&o)
	}

	app := fiber.New(fiber.Config{
		AppName: "eprocurement-gateway",
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		},
	})

	app.Use(requestLogger(log))
	app.Use(traceID())

	app.Get("/healthz", healthz(verifier))
	app.Get("/.well-known/jwks.json", jwks(verifier))

	v1 := app.Group("/api/v1", bearerAuth(verifier))
	v1.Get("/me", me)
	if o.procurement != nil {
		registerVendorRoutes(app, verifier, o.procurement)
	}

	return app
}

// healthz reports readiness: the process is up and the identity key set is
// reachable (fresh cache or successful refresh).
func healthz(verifier *token.Verifier) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.UserContext(), 2*time.Second)
		defer cancel()
		if _, err := verifier.KeySet(ctx); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "unavailable", "error": "identity unreachable"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	}
}

// jwkJSON is the standard JWKS shape for an Ed25519 key (RFC 7517 + OKP).
type jwkJSON struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	X   string `json:"x"` // raw public key, base64url
}

func jwks(verifier *token.Verifier) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.UserContext(), 2*time.Second)
		defer cancel()
		keys, err := verifier.KeySet(ctx)
		if err != nil {
			return fiber.NewError(fiber.StatusServiceUnavailable, "identity unreachable")
		}
		out := make([]jwkJSON, 0, len(keys))
		for _, k := range keys {
			out = append(out, jwkJSON{
				Kty: "OKP",
				Crv: "Ed25519",
				Use: "sig",
				Alg: "EdDSA",
				Kid: k.Kid,
				X:   base64.RawURLEncoding.EncodeToString(k.Ed25519),
			})
		}
		c.Set("Cache-Control", "public, max-age=60")
		return c.JSON(fiber.Map{"keys": out})
	}
}

// bearerAuth verifies "Authorization: Bearer <token>" and stores the claims
// for downstream handlers. Missing/invalid/expired tokens are 401 with a
// generic body; an unreachable identity is 503.
func bearerAuth(verifier *token.Verifier) fiber.Handler {
	return func(c *fiber.Ctx) error {
		header := c.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		claims, err := verifier.Verify(c.UserContext(), strings.TrimPrefix(header, prefix))
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid or expired token"})
		}
		c.Locals("claims", claims)
		return c.Next()
	}
}

// me demonstrates verified access: it echoes the token's claims. Real
// business endpoints replace it in later issues.
func me(c *fiber.Ctx) error {
	claims := c.Locals("claims").(auth.AccessClaims)
	return c.JSON(fiber.Map{
		"user_id":   claims.Sub,
		"tenant_id": claims.TenantID,
		"roles":     claims.Roles,
	})
}

// requestLogger logs one JSON line per request with method, path, status,
// duration, and the request's trace id.
func requestLogger(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		log.Info("http request",
			"method", c.Method(),
			"path", c.Path(),
			"status", c.Response().StatusCode(),
			"duration_ms", time.Since(start).Milliseconds(),
			"trace_id", traceIDFrom(c),
		)
		return err
	}
}

// traceID propagates X-Trace-Id (generating one when absent) into locals and
// the response header, mirroring the gRPC interceptor on the identity side.
func traceID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := strings.TrimSpace(c.Get("X-Trace-Id"))
		if id == "" || len(id) > 64 {
			id = logger.NewTraceID()
		}
		c.Locals("trace_id", id)
		c.Set("X-Trace-Id", id)
		return c.Next()
	}
}

func traceIDFrom(c *fiber.Ctx) string {
	id, _ := c.Locals("trace_id").(string)
	return id
}
