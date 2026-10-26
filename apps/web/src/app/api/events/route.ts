import { NextResponse } from "next/server";
import { cookies } from "next/headers";
import { ACCESS_COOKIE } from "@/lib/constants";
import { eventsToSearchParams } from "@/lib/events-query";
import { listEvents } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// GET /api/events?entityType=&entityId=&limit=&cursor= — thin BFF proxy for
// the event history timeline (issue #43). The access token never leaves the
// server (httpOnly cookie); the client only ever talks to this route.
// 401 from the gateway (expired access token) passes through as 401 — the
// client then runs the refresh dance; 400 passes through with the gateway's
// machine code intact (events_filter_invalid / events_cursor_invalid); any
// other failure surfaces as 502 for the UI's error-with-retry state.
export async function GET(request: Request) {
  const store = await cookies();
  const accessToken = store.get(ACCESS_COOKIE)?.value;
  if (!accessToken) {
    return NextResponse.json({ error: "Sign-in required.", code: "unauthorized" }, { status: 401 });
  }

  const query = eventsToSearchParams(new URL(request.url).searchParams).toString();
  const result = await listEvents(accessToken, query);
  if (!result.ok) {
    if (result.status === 401) {
      return NextResponse.json({ error: "Session expired.", code: "unauthorized" }, { status: 401 });
    }
    if (result.status === 400) {
      return NextResponse.json(
        { error: "Invalid events query.", code: result.code ?? "events_query_invalid" },
        { status: 400 },
      );
    }
    return NextResponse.json(
      { error: "Failed to load events from the gateway.", code: "gateway_unavailable" },
      { status: 502 },
    );
  }
  return NextResponse.json(result.data);
}
