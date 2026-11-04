import { NextResponse } from "next/server";
import { cookies } from "next/headers";
import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { ACCESS_COOKIE } from "@/lib/constants";
import { createRequisitionDraft, listRequisitions } from "@/lib/gateway";
import { filtersToSearchParams, parseFilters } from "@/lib/requisition-filters";

export const dynamic = "force-dynamic";

// GET /api/requisitions?q=&status=&page=&page_size= — thin BFF proxy for the
// requisition list (issue #32). The access token never leaves the server
// (httpOnly cookie); the client only ever talks to this route. Query params
// are canonicalized through requisition-filters before forwarding, so
// garbage never reaches the gateway. 401 from the gateway (expired access
// token) is passed through as 401 — the client then navigates to the
// refresh dance, which rotates the cookies and bounces back; any other
// failure surfaces as 502 for the UI's error-with-retry state.
export async function GET(request: Request) {
  const store = await cookies();
  const accessToken = store.get(ACCESS_COOKIE)?.value;
  if (!accessToken) {
    return NextResponse.json({ error: "Sign-in required.", code: "unauthorized" }, { status: 401 });
  }

  const filters = parseFilters(new URL(request.url).searchParams);
  const result = await listRequisitions(accessToken, filtersToSearchParams(filters).toString());
  if (!result.ok) {
    if (result.status === 401) {
      return NextResponse.json({ error: "Session expired.", code: "unauthorized" }, { status: 401 });
    }
    return NextResponse.json(
      { error: "Failed to load requisitions from the gateway.", code: "gateway_unavailable" },
      { status: 502 },
    );
  }
  return NextResponse.json(result.data);
}

// POST /api/requisitions — create the wizard's server-side draft (#33).
// Unlike the list proxy above, the typed result is forwarded: gateway 4xx
// bodies (machine codes like requisition_currency_invalid) ride through
// verbatim so the wizard can map them; only 5xx/network collapse to 502.
export async function POST(request: Request) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const body = (await request.json().catch(() => null)) as { currency?: string } | null;
  if (!body || typeof body.currency !== "string" || !["IDR", "USD"].includes(body.currency)) {
    return NextResponse.json(
      { error: "currency must be IDR or USD", code: "requisition_currency_invalid" },
      { status: 400 },
    );
  }
  const result = await createRequisitionDraft(tokenOr401, body.currency);
  return forwardGatewayResult(result, 201);
}
