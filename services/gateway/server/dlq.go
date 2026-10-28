package server

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/dlq"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// DLQAdmin is the broker surface the DLQ routes need (issue #48): the
// gateway's small AMQP client over procurement.dlq; tests substitute a
// fake. The concrete client lives in services/gateway/dlq — the gateway
// holding broker access is the maintainer's option-A decision of
// 2026-10-28 (issue #48 comments).
type DLQAdmin interface {
	List(ctx context.Context, tenantID string, limit int) (dlq.ListResult, error)
	Replay(ctx context.Context, tenantID, messageID string) error
}

// registerDLQRoutes wires the admin DLQ surface (#48; PRD §14.5 "admin
// visible", §16 "DLQ monitor — admin endpoint"): GET /api/v1/dlq lists the
// poison messages (the caller's decoded entries plus undecodable entries
// as cross-tenant metadata) and POST /api/v1/dlq/replay moves one back to
// its work queue. Both are dlq:manage — admin-only, like
// approval:configure/match:configure. The route follows the
// match-settings precedent: /api/v1/dlq, no /admin prefix.
func registerDLQRoutes(app *fiber.App, verifier *token.Verifier, admin DLQAdmin) {
	group := app.Group("/api/v1/dlq", bearerAuth(verifier))
	group.Get("/", RequirePermission(rbac.DLQManage), listDLQ(admin))
	group.Post("/replay", RequirePermission(rbac.DLQManage), replayDLQ(admin))
}

// DLQ list defaults: limit=50, capped at 500 (the drain itself is capped
// at dlq.DefaultScanCap regardless — limit only bounds the response).
const (
	dlqDefaultLimit = 50
	dlqMaxLimit     = 500
)

// listDLQ serves GET /api/v1/dlq?limit=. The tenant never comes from the
// query string: decoded entries are filtered by the verified claims'
// tenant; undecodable entries cannot be attributed and appear to every
// tenant as metadata only (decode_error).
func listDLQ(admin DLQAdmin) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		limit := dlqDefaultLimit
		if n, err := strconv.Atoi(strings.TrimSpace(c.Query("limit"))); err == nil && n > 0 {
			limit = min(n, dlqMaxLimit)
		}
		res, err := admin.List(c.UserContext(), claims.TenantID, limit)
		if err != nil {
			return dlqError(c, err)
		}
		out := make([]fiber.Map, 0, len(res.Messages))
		for _, m := range res.Messages {
			out = append(out, dlqMessageJSON(m))
		}
		return c.JSON(fiber.Map{"depth": res.Depth, "scanned": res.Scanned, "messages": out})
	}
}

// dlqMessageJSON shapes one entry: decoded entries carry the business
// fields; decode_error entries carry metadata only — no payload, no
// tenant (the body does not decode, so the tenant cannot be known; the
// deliberate trade-off is documented in docs/ops/dlq-monitor.md).
func dlqMessageJSON(m dlq.Message) fiber.Map {
	if m.DecodeError {
		return fiber.Map{
			"message_id":     m.MessageID,
			"decode_error":   true,
			"retry_count":    m.RetryCount,
			"original_queue": m.OriginalQueue,
			"reason":         m.Reason,
			"died_at":        m.DiedAt,
		}
	}
	return fiber.Map{
		"message_id":     m.MessageID,
		"decode_error":   false,
		"event_id":       m.EventID,
		"tenant_id":      m.TenantID,
		"type":           m.Type,
		"occurred_at":    m.OccurredAt,
		"retry_count":    m.RetryCount,
		"original_queue": m.OriginalQueue,
		"reason":         m.Reason,
		"died_at":        m.DiedAt,
	}
}

// replayBody is the replay request: message_id is the AMQP MessageId the
// publisher stamps with the event_id (publisher.go) — a property, so it
// survives a broken body and is the only handle an undecodable entry has.
type replayBody struct {
	MessageID string `json:"message_id"`
}

// replayDLQ serves POST /api/v1/dlq/replay: the message returns to its
// original work queue with a fresh retry ladder (x-retry-count reset to
// 0 — maintainer decision 2026-10-28). Another tenant's message answers
// 404 (cross-tenant opacity); an undecodable message answers 409 (it can
// never succeed — removal goes through broker tooling, docs/ops).
func replayDLQ(admin DLQAdmin) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		var body replayBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body", "code": "body_invalid"})
		}
		messageID := strings.TrimSpace(body.MessageID)
		if messageID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "message_id is required", "code": "dlq_message_id_invalid",
			})
		}
		if err := admin.Replay(c.UserContext(), claims.TenantID, messageID); err != nil {
			return dlqError(c, err)
		}
		return c.JSON(fiber.Map{"replayed": messageID})
	}
}

// dlqMessages renders the client's typed failures as human sentences.
var dlqMessages = map[string]string{
	"dlq_broker_unavailable":     "message broker unreachable",
	"dlq_message_not_found":      "no such DLQ message for this tenant",
	"dlq_message_not_replayable": "message cannot be replayed (permanently invalid)",
}

// dlqError maps the dlq client's typed errors to HTTP — these are local
// errors, not proxied gRPC, so the mapping is explicit rather than
// grpcHTTPError.
func dlqError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, dlq.ErrMessageNotFound):
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": dlqMessages["dlq_message_not_found"], "code": "dlq_message_not_found",
		})
	case errors.Is(err, dlq.ErrNotReplayable):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": dlqMessages["dlq_message_not_replayable"], "code": "dlq_message_not_replayable",
		})
	case errors.Is(err, dlq.ErrBrokerUnavailable):
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": dlqMessages["dlq_broker_unavailable"], "code": "dlq_broker_unavailable",
		})
	default:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "dlq operation failed", "code": "dlq_internal",
		})
	}
}
