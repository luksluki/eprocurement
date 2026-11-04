package server

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerAuditRoutes wires the audit admin view (#44, PRD §14.7): the
// decision log listing under GET /api/v1/audit and the CSV export under
// GET /api/v1/audit/export. Both are audit:read — viewer/admin at the
// edge (the maintainer decision on issue #44); documents re-checks the
// same permission in its service-layer interceptor map (defense in depth).
func registerAuditRoutes(app *fiber.App, verifier *token.Verifier, documents DocumentsClient) {
	audit := app.Group("/api/v1/audit", bearerAuth(verifier))
	audit.Get("/", RequirePermission(rbac.AuditRead), listAudit(documents))
	audit.Get("/export", RequirePermission(rbac.AuditRead), exportAudit(documents))
}

// listAudit serves GET /api/v1/audit?action=&entityType=&entityId=&limit=&cursor=.
// The tenant never comes from the query string: it is taken from the
// verified claims and injected as gRPC metadata; documents scopes the Mongo
// query by it. action is one of the five audit verbs (approve, reject,
// issue, post, match) or empty; the entity pair must be both set or both
// empty; limit and cursor pass through untouched — documents clamps the
// limit (default 20, max 100) and validates the keyset cursor (the events
// route conventions).
func listAudit(documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		limit, err := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
		if err != nil {
			limit = 0 // documents normalizes non-numeric limits to the default
		}
		resp, err := documents.ListAudit(forwardContext(c, claims), &documentsv1.ListAuditRequest{
			Action:     strings.TrimSpace(c.Query("action")),
			EntityType: strings.TrimSpace(c.Query("entityType")),
			EntityId:   strings.TrimSpace(c.Query("entityId")),
			Limit:      int32(limit),
			NextCursor: strings.TrimSpace(c.Query("cursor")),
		})
		if err != nil {
			return auditError(c, err)
		}
		out := make([]fiber.Map, 0, len(resp.GetEntries()))
		for _, e := range resp.GetEntries() {
			out = append(out, auditEntryJSON(e))
		}
		return c.JSON(fiber.Map{"entries": out, "next_cursor": resp.GetNextCursor()})
	}
}

// exportAudit serves GET /api/v1/audit/export: the tenant's audit log as a
// text/csv attachment (RFC 4180, CRLF). The CSV is bounded by contract —
// documents caps it at the newest 10 000 rows and renders metadata columns
// only (no details/payload), so the unary gRPC response stays a few MB at
// worst against the 16 MiB receive limit main.go sets on the documents
// dial. Streaming/MinIO delivery is the documented follow-up if the cap
// must rise (#49 precedent).
func exportAudit(documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		resp, err := documents.ExportAudit(forwardContext(c, claims), &documentsv1.ExportAuditRequest{})
		if err != nil {
			return auditError(c, err)
		}
		c.Set("Content-Disposition", `attachment; filename="audit-log.csv"`)
		c.Set("Cache-Control", "no-store")
		c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
		return c.Status(fiber.StatusOK).Send(resp.GetCsv())
	}
}

// auditMessages renders the machine codes of documents' audit area as
// human sentences; unknown messages pass through verbatim.
var auditMessages = map[string]string{
	"audit_action_invalid":  "action must be one of approve, reject, issue, post, match",
	"events_filter_invalid": "entityType and entityId must both be set or both empty",
	"events_cursor_invalid": "cursor is not a valid continuation token",
}

// auditError maps documents' gRPC failures to HTTP with the machine code
// preserved (same shape as attachmentError).
func auditError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, auditMessages)
}

// auditEntryJSON is the REST shape of one audit row — the bounded column
// set of the wire (details never crosses; ListEvents payload_json carries
// the same payload for the timeline).
func auditEntryJSON(e *documentsv1.AuditEntry) fiber.Map {
	return fiber.Map{
		"event_id":    e.GetEventId(),
		"tenant_id":   e.GetTenantId(),
		"actor_id":    e.GetActorId(),
		"actor_ip":    e.GetActorIp(),
		"action":      e.GetAction(),
		"entity_type": e.GetEntityType(),
		"entity_id":   e.GetEntityId(),
		"trace_id":    e.GetTraceId(),
		"occurred_at": e.GetOccurredAt(),
		"recorded_at": e.GetRecordedAt(),
	}
}
