package server

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	documentsv1 "github.com/luksluki/eprocurement/gen/go/documents/v1"
	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/gateway/token"
)

// registerExportRoutes wires the export surface (#49 + #53, FR-10): the
// run history under GET /api/v1/exports/runs and the CSV download URL under
// GET /api/v1/exports/runs/:id/download-url (both export:run —
// finance/admin at the edge; documents re-checks the same permission at the
// service layer, defense in depth), and the manual trigger under POST
// /api/v1/exports (export:trigger — admin-only; procurement re-checks it).
// Each half nil-guards its backend: an app wired without documents exposes
// only the trigger, one without procurement only the reads — no path
// panics on a nil interface.
func registerExportRoutes(app *fiber.App, verifier *token.Verifier, documents DocumentsClient, procurement ProcurementClient) {
	exports := app.Group("/api/v1/exports", bearerAuth(verifier))
	if documents != nil {
		exports.Get("/runs", RequirePermission(rbac.ExportRun), listExportRuns(documents))
		exports.Get("/runs/:id/download-url", RequirePermission(rbac.ExportRun), exportDownloadURL(documents))
	}
	if procurement != nil {
		exports.Post("/", RequirePermission(rbac.ExportTrigger), triggerExport(procurement))
	}
}

// triggerExport serves POST /api/v1/exports (#53): record one manual
// export request for the caller's tenant. The answer is 202 Accepted — the
// request is queued (procurement writes the export.requested outbox row;
// the worker's exports consumer runs it), never executed inline. event_id
// correlates the request with the queued event.
func triggerExport(procurement ProcurementClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		resp, err := procurement.TriggerExport(forwardContext(c, claims),
			&procurementv1.TriggerExportRequest{})
		if err != nil {
			return exportError(c, err)
		}
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"status":   "requested",
			"event_id": resp.GetEventId(),
		})
	}
}

// listExportRuns serves GET /api/v1/exports/runs?limit=. The tenant never
// comes from the query string: it is taken from the verified claims and
// injected as gRPC metadata; documents scopes the Mongo query by it. limit
// passes through untouched — documents clamps it (default 20, max 100).
func listExportRuns(documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		limit, err := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
		if err != nil {
			limit = 0 // documents normalizes non-numeric limits to the default
		}
		resp, err := documents.ListExportRuns(forwardContext(c, claims),
			&documentsv1.ListExportRunsRequest{Limit: int32(limit)})
		if err != nil {
			return exportError(c, err)
		}
		runs := make([]fiber.Map, 0, len(resp.GetRuns()))
		for _, r := range resp.GetRuns() {
			runs = append(runs, exportRunJSON(r))
		}
		return c.JSON(fiber.Map{"runs": runs})
	}
}

// exportDownloadURL serves GET /api/v1/exports/runs/:id/download-url: the
// presigned GET for one successful run's CSV. object_key never appears in
// the response — the URL is the download path (the attachment convention).
func exportDownloadURL(documents DocumentsClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, ok := claimsOf(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing bearer token"})
		}
		resp, err := documents.GetExportDownloadUrl(forwardContext(c, claims),
			&documentsv1.GetExportDownloadUrlRequest{RunId: c.Params("id")})
		if err != nil {
			return exportError(c, err)
		}
		out := exportRunJSON(resp.GetRun())
		out["download"] = fiber.Map{"url": resp.GetDownloadUrl(), "expires_at": resp.GetExpiresAt()}
		return c.JSON(out)
	}
}

// exportMessages renders the machine codes of documents' export area as
// human sentences; unknown messages pass through verbatim.
var exportMessages = map[string]string{
	"export_run_id_invalid": "run id is malformed",
	"export_run_not_found":  "export run not found",
	"export_not_succeeded":  "export run has no file — it failed or is still pending",
}

// exportError maps documents' gRPC failures to HTTP with the machine code
// preserved (same shape as attachmentError).
func exportError(c *fiber.Ctx, err error) error {
	return grpcHTTPError(c, err, exportMessages)
}

// exportRunJSON is the REST shape of one run record. object_key stays
// internal; missed_previous surfaces for the run-history warning badge;
// trigger surfaces for the #53 manual badge ("" on pre-#53 rows renders as
// scheduled).
func exportRunJSON(r *documentsv1.ExportRun) fiber.Map {
	return fiber.Map{
		"run_id":          r.GetRunId(),
		"tenant_id":       r.GetTenantId(),
		"scheduled_for":   r.GetScheduledFor(),
		"status":          r.GetStatus(),
		"trigger":         r.GetTrigger(),
		"row_count":       r.GetRowCount(),
		"missed_previous": r.GetMissedPrevious(),
		"error_message":   r.GetErrorMessage(),
		"started_at":      r.GetStartedAt(),
		"finished_at":     r.GetFinishedAt(),
	}
}
