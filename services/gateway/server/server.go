// Package server wires the gateway's GoFiber app: health, the public JWKS
// endpoint, Bearer-token verification against the identity JWKS cache, a
// minimal authenticated endpoint (/api/v1/me) that demonstrates
// verification, the proxied business REST routes, and the admin DLQ
// inspection/replay routes (/api/v1/dlq — the gateway's one direct
// broker-backed surface).
package server

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/services/gateway/token"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// Option adds a proxied service to the gateway.
type Option func(*gatewayOptions)

// ProcurementClient is the full procurement surface the gateway proxies: the
// vendor API, the requisition create/edit/submit/read API, the approval
// decide/rules API, the PO conversion API, the PO lifecycle actions, and the
// goods-receipt posting API. The generated gRPC client satisfies it; tests
// substitute a fake.
type ProcurementClient interface {
	VendorClient
	CreateRequisition(ctx context.Context, in *procurementv1.CreateRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error)
	UpdateRequisition(ctx context.Context, in *procurementv1.UpdateRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error)
	SubmitRequisition(ctx context.Context, in *procurementv1.SubmitRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error)
	ListRequisitions(ctx context.Context, in *procurementv1.ListRequisitionsRequest, opts ...grpc.CallOption) (*procurementv1.ListRequisitionsResponse, error)
	GetRequisition(ctx context.Context, in *procurementv1.GetRequisitionRequest, opts ...grpc.CallOption) (*procurementv1.Requisition, error)
	DecideApproval(ctx context.Context, in *procurementv1.DecideApprovalRequest, opts ...grpc.CallOption) (*procurementv1.DecideApprovalResponse, error)
	GetApprovalRules(ctx context.Context, in *procurementv1.GetApprovalRulesRequest, opts ...grpc.CallOption) (*procurementv1.ApprovalRulesResponse, error)
	ReplaceApprovalRules(ctx context.Context, in *procurementv1.ReplaceApprovalRulesRequest, opts ...grpc.CallOption) (*procurementv1.ApprovalRulesResponse, error)
	CreatePurchaseOrder(ctx context.Context, in *procurementv1.CreatePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.CreatePurchaseOrderResponse, error)
	IssuePurchaseOrder(ctx context.Context, in *procurementv1.IssuePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error)
	AcknowledgePurchaseOrder(ctx context.Context, in *procurementv1.AcknowledgePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error)
	CancelPurchaseOrder(ctx context.Context, in *procurementv1.CancelPurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error)
	ClosePurchaseOrder(ctx context.Context, in *procurementv1.ClosePurchaseOrderRequest, opts ...grpc.CallOption) (*procurementv1.PurchaseOrder, error)
	PostGoodsReceipt(ctx context.Context, in *procurementv1.PostGoodsReceiptRequest, opts ...grpc.CallOption) (*procurementv1.PostGoodsReceiptResponse, error)
	CreateInvoice(ctx context.Context, in *procurementv1.CreateInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error)
	GetInvoice(ctx context.Context, in *procurementv1.GetInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error)
	MatchInvoice(ctx context.Context, in *procurementv1.MatchInvoiceRequest, opts ...grpc.CallOption) (*procurementv1.Invoice, error)
	GetMatchSettings(ctx context.Context, in *procurementv1.GetMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error)
	ReplaceMatchSettings(ctx context.Context, in *procurementv1.ReplaceMatchSettingsRequest, opts ...grpc.CallOption) (*procurementv1.MatchSettings, error)
}

type gatewayOptions struct {
	procurement ProcurementClient
	documents   DocumentsClient
	identity    IdentityClient
	loginLimit  *loginLimiter
	dlq         DLQAdmin
}

// WithProcurement proxies the procurement API under /api/v1/vendors,
// /api/v1/requisitions, /api/v1/approvals, /api/v1/purchase-orders,
// /api/v1/invoices, and /api/v1/match-settings.
func WithProcurement(client ProcurementClient) Option {
	return func(o *gatewayOptions) { o.procurement = client }
}

// WithDocuments adds attachment upload/download under
// /api/v1/vendors/:id/attachments, embeds attachment metadata in vendor
// detail responses, and serves the event history under /api/v1/events (the
// events route needs documents only; the vendor routes need
// WithProcurement).
func WithDocuments(client DocumentsClient) Option {
	return func(o *gatewayOptions) { o.documents = client }
}

// WithIdentity proxies identity's auth API under the public /api/v1/auth
// routes (login/refresh/logout, no Bearer required). loginRatePerMin bounds
// login per client IP; <=0 falls back to the default budget.
func WithIdentity(client IdentityClient, loginRatePerMin int) Option {
	return func(o *gatewayOptions) {
		o.identity = client
		o.loginLimit = newLoginLimiter(loginRatePerMin)
	}
}

// WithDLQ adds the admin DLQ surface under /api/v1/dlq (#48): GET lists
// the poison messages for the caller's tenant and POST /replay returns
// one to its work queue. admin is the gateway's own AMQP client over
// procurement.dlq (services/gateway/dlq), not a proxied gRPC service.
func WithDLQ(admin DLQAdmin) Option {
	return func(o *gatewayOptions) { o.dlq = admin }
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

	// /api/v1/me stays behind Bearer auth. Note: a group-level middleware
	// here (app.Group("/api/v1", bearerAuth(...))) would also swallow the
	// public /api/v1/auth/* routes — Fiber group middleware matches by
	// prefix — so protected routes attach the verifier per route/group of
	// their own (see vendors/requisitions/auth registrations).
	app.Get("/api/v1/me", bearerAuth(verifier), me)
	if o.identity != nil {
		// Public auth routes: no Bearer required (ADR-0001 — login is the
		// only identity surface exposed to clients).
		registerAuthRoutes(app, o.identity, o.loginLimit)
	}
	if o.procurement != nil {
		registerVendorRoutes(app, verifier, o.procurement, o.documents)
		registerRequisitionRoutes(app, verifier, o.procurement, o.documents)
		registerApprovalRoutes(app, verifier, o.procurement)
		registerPurchaseOrderRoutes(app, verifier, o.procurement)
		registerInvoiceRoutes(app, verifier, o.procurement, o.documents)
		registerMatchSettingsRoutes(app, verifier, o.procurement)
	}
	if o.documents != nil {
		registerEventRoutes(app, verifier, o.documents)
	}
	if o.dlq != nil {
		registerDLQRoutes(app, verifier, o.dlq)
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
