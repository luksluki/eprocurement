// BFF plumbing for the requisition-form routes (#33). Two jobs: read the
// httpOnly access cookie (never exposed to the client) and translate a
// gateway call outcome into the route response. Translation contract:
// 401 stays 401 (the client runs the refresh dance), every other 4xx rides
// through verbatim with its machine code so the wizard can map it
// per-field/per-step, and anything else becomes the retryable 502.
import { NextResponse } from "next/server";
import { cookies } from "next/headers";
import { ACCESS_COOKIE } from "./constants";
import type { GatewayResult } from "./gateway";

// accessTokenOr401 returns the session's access token, or a ready-to-return
// 401 response when there is no session.
export async function accessTokenOr401(): Promise<string | NextResponse> {
  const store = await cookies();
  const token = store.get(ACCESS_COOKIE)?.value;
  if (!token) {
    return NextResponse.json({ error: "Sign-in required.", code: "unauthorized" }, { status: 401 });
  }
  return token;
}

export function isNextResponse(value: unknown): value is NextResponse {
  return value instanceof NextResponse;
}

// forwardGatewayResult renders a gateway result as the BFF response.
export function forwardGatewayResult<T>(result: GatewayResult<T>, successStatus = 200): NextResponse {
  if (result.ok) {
    return NextResponse.json(result.data, { status: successStatus });
  }
  if (result.status === 401) {
    return NextResponse.json({ error: "Session expired.", code: "unauthorized" }, { status: 401 });
  }
  if (result.status >= 400 && result.status < 500) {
    return NextResponse.json({ error: result.error, code: result.code }, { status: result.status });
  }
  return NextResponse.json(
    { error: "Failed to reach the gateway.", code: "gateway_unavailable" },
    { status: 502 },
  );
}
