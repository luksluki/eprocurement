import { accessTokenOr401, isNextResponse } from "@/lib/bff";
import { gatewayUrl } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// GET /api/requisitions/[id]/attachments/[attachmentId]/download — mint a
// short-lived presigned GET URL for a ready attachment (#33). The BFF adds
// nothing: it exists so the browser never holds the access token while
// talking to MinIO. 412 attachment_not_ready and 404 ride through verbatim.
export async function GET(
  _request: Request,
  { params }: { params: Promise<{ id: string; attachmentId: string }> },
) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id, attachmentId } = await params;
  const target = `${gatewayUrl()}/api/v1/requisitions/${encodeURIComponent(id)}/attachments/${encodeURIComponent(attachmentId)}/download`;
  try {
    const res = await fetch(target, {
      headers: { authorization: `Bearer ${tokenOr401}` },
      cache: "no-store",
    });
    if (!res.ok) {
      const parsed = (await res.json().catch(() => null)) as { error?: string; code?: string } | null;
      if (res.status === 401) {
        return Response.json({ error: "Session expired.", code: "unauthorized" }, { status: 401 });
      }
      if (res.status >= 400 && res.status < 500) {
        return Response.json(
          { error: parsed?.error ?? "download failed", code: parsed?.code ?? "gateway_error" },
          { status: res.status },
        );
      }
      return Response.json(
        { error: "Failed to reach the gateway.", code: "gateway_unavailable" },
        { status: 502 },
      );
    }
    return new Response(res.body, {
      status: 200,
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  } catch {
    return Response.json(
      { error: "Failed to reach the gateway.", code: "gateway_unavailable" },
      { status: 502 },
    );
  }
}
