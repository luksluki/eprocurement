import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { listExportRuns } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// GET /api/exports/runs — thin BFF proxy for the export run history
// (issue #49, FR-10). The access token never leaves the server (httpOnly
// cookie); the client only ever talks to this route. 401 from the gateway
// passes through as 401 (the client runs the refresh dance); 403
// export:run and every other 4xx ride through with the machine code; any
// other failure surfaces as the retryable 502.
export async function GET() {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const result = await listExportRuns(tokenOr401);
  return forwardGatewayResult(result);
}
