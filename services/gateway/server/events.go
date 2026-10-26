package server

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerEventRoutes wires GET /api/v1/events (PRD §15, FR-11): Bearer auth
// plus requisition:read — the exact permission documents re-checks on
// ListEvents (services/documents/server/server.go methodPermissions), so the
// edge and the service agree on the audience (dev-note 042).
func registerEventRoutes(app *fiber.App, verifier *token.Verifier, documents DocumentsClient) {
	events := app.Group("/api/v1/events", bearerAuth(verifier))
	events.Get("/", RequirePermission(rbac.RequisitionRead), listEvents(documents))
}

// listEvents serves GET /api/v1/events?entityType=&entityId=&limit=&cursor=.
// The filter params are camelCase because PRD §15 spells the route that way —
// a deliberate deviation from the snake_case params of the other routes
// (dev-note 043). The tenant never comes from the query string: it is taken
// from the verified claims and injected as gRPC metadata; documents scopes
// the Mongo query by it. limit and cursor pass through untouched — documents
// clamps the limit (default 20, max 100) and validates the keyset cursor.
func listEvents(documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		limit, err := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
		if err != nil {
			limit = 0 // documents normalizes non-numeric limits to the default
		}
		resp, err := documents.ListEvents(forwardContext(c, claims), &documentsv1.ListEventsRequest{
			EntityType: strings.TrimSpace(c.Query("entityType")),
			EntityId:   strings.TrimSpace(c.Query("entityId")),
			Limit:      int32(limit),
			NextCursor: strings.TrimSpace(c.Query("cursor")),
		})
		if err != nil {
			return eventError(c, err)
		}
		out := make([]fiber.Map, 0, len(resp.GetEvents()))
		for _, ev := range resp.GetEvents() {
			out = append(out, eventJSON(ev))
		}
		return c.JSON(fiber.Map{"events": out, "next_cursor": resp.GetNextCursor()})
	}
}

// eventMessages renders the machine codes of documents' history store as
// human sentences; unknown messages pass through verbatim.
var eventMessages = map[string]string{
	"events_filter_invalid":   "entityType and entityId must both be set or both empty",
	"events_cursor_invalid":   "cursor is not a valid continuation token",
	"events_event_id_invalid": "event id is malformed",
	"events_event_incomplete": "event envelope is incomplete",
	"events_payload_invalid":  "event payload is not valid JSON",
}

// eventError maps documents' gRPC failures to HTTP with the machine code
// preserved (same shape as attachmentError).
func eventError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, eventMessages)
}

// eventJSON is the REST shape of one history event — the PRD §14.5 envelope
// with payload_json verbatim (no restructuring, dev-note 042).
func eventJSON(ev *documentsv1.Event) fiber.Map {
	return fiber.Map{
		"event_id":     ev.GetEventId(),
		"tenant_id":    ev.GetTenantId(),
		"type":         ev.GetType(),
		"version":      ev.GetVersion(),
		"occurred_at":  ev.GetOccurredAt(),
		"actor_id":     ev.GetActorId(),
		"trace_id":     ev.GetTraceId(),
		"entity_type":  ev.GetEntityType(),
		"entity_id":    ev.GetEntityId(),
		"payload_json": ev.GetPayloadJson(),
	}
}
