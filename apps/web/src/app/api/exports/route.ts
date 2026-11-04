import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { triggerExport } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/exports — thin BFF proxy for the manual export trigger
// (issue #53, FR-10). The access token never leaves the server (httpOnly
// cookie); the client only ever talks to this route. The gateway answers
// 202 {status:"requested",event_id} — the run is queued, not executed
// inline. 401 from the gateway passes through as 401 (the client runs the
// refresh dance); 403 export:trigger (admin-only) rides through with its
// machine code; any other failure surfaces as the retryable 502.
export async function POST() {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const result = await triggerExport(tokenOr401);
  // 202 mirrors the gateway: the trigger is queued, not executed inline.
  return forwardGatewayResult(result, 202);
}
