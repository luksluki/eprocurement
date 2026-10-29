import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { getExportDownloadUrl } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// GET /api/exports/runs/[id]/download-url — mint a short-lived presigned
// GET for one successful run's CSV (issue #49, FR-10). The BFF adds
// nothing: it exists so the browser never holds the access token while
// talking to MinIO. 412 export_not_succeeded (failed run) and 404
// export_run_not_found (unknown or cross-tenant id) ride through verbatim
// for the table's per-row state.
export async function GET(_request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const result = await getExportDownloadUrl(tokenOr401, id);
  return forwardGatewayResult(result);
}
