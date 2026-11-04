// Query canonicalization for /api/events (issue #43). Pure so the BFF route
// stays a thin proxy and the normalization is unit-testable — same pattern as
// requisition-filters. Keeps the PRD §15 params (camelCase entityType /
// entityId, limit, cursor), trims strings, drops empties, and drops a
// non-positive/non-numeric limit — garbage never reaches the gateway.
export function eventsToSearchParams(params: URLSearchParams): URLSearchParams {
  const out = new URLSearchParams();
  for (const key of ["entityType", "entityId", "cursor"]) {
    const value = (params.get(key) ?? "").trim();
    if (value) {
      out.set(key, value);
    }
  }
  const limit = Number((params.get("limit") ?? "").trim());
  if (Number.isInteger(limit) && limit > 0) {
    out.set("limit", String(limit));
  }
  return out;
}
