import { NextResponse } from "next/server";
import { cookies } from "next/headers";
import { ACCESS_COOKIE } from "@/lib/constants";
import { listAudit } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// The audit filter params the BFF forwards — camelCase, the events-route
// convention (dev-note 043; the gateway route spells them the same way).
const FILTER_KEYS = ["action", "entityType", "entityId", "limit", "cursor"] as const;

function toQueryParams(incoming: URLSearchParams): URLSearchParams {
  const out = new URLSearchParams();
  for (const key of FILTER_KEYS) {
    if (key === "limit") {
      // Positive integers only — documents clamps the rest; garbage never
      // forwards (the events-query convention).
      const raw = incoming.get("limit");
      if (raw !== null && /^\d+$/.test(raw.trim()) && Number(raw) > 0) {
        out.set("limit", raw.trim());
      }
      continue;
    }
    const value = incoming.get(key);
    if (value !== null && value.trim() !== "") {
      out.set(key, value.trim());
    }
  }
  return out;
}

// GET /api/audit?action=&entityType=&entityId=&limit=&cursor= — thin BFF
// proxy for the decision audit log (#44). The access token never leaves the
// server (httpOnly cookie); the client only ever talks to this route.
// 401 from the gateway (expired access token) passes through as 401 — the
// client then runs the refresh dance; 400 passes through with the gateway's
// machine code intact (audit_action_invalid / events_filter_invalid /
// events_cursor_invalid); any other failure surfaces as 502.
export async function GET(request: Request) {
  const store = await cookies();
  const accessToken = store.get(ACCESS_COOKIE)?.value;
  if (!accessToken) {
    return NextResponse.json({ error: "Sign-in required.", code: "unauthorized" }, { status: 401 });
  }

  const query = toQueryParams(new URL(request.url).searchParams).toString();
  const result = await listAudit(accessToken, query);
  if (!result.ok) {
    if (result.status === 401) {
      return NextResponse.json({ error: "Session expired.", code: "unauthorized" }, { status: 401 });
    }
    if (result.status === 400) {
      return NextResponse.json(
        { error: "Invalid audit query.", code: result.code ?? "audit_query_invalid" },
        { status: 400 },
      );
    }
    if (result.status === 403) {
      return NextResponse.json(
        { error: "The audit log requires the audit:read permission.", code: "permission_denied" },
        { status: 403 },
      );
    }
    return NextResponse.json(
      { error: "Failed to load the audit log from the gateway.", code: "gateway_unavailable" },
      { status: 502 },
    );
  }
  return NextResponse.json(result.data);
}
