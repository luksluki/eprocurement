import { NextResponse } from "next/server";
import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { getRequisition, replaceRequisitionItems, type RequisitionItemWire } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// GET /api/requisitions/[id] — draft restore (#33): header, items, and the
// embedded attachment metadata in one round trip. Gateway 4xx bodies ride
// through verbatim (403 requisition_forbidden / 404 requisition_not_found /
// 409 non-draft) so the wizard can stop autosave and render the error state.
export async function GET(_request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const result = await getRequisition(tokenOr401, id);
  return forwardGatewayResult(result);
}

// PUT /api/requisitions/[id] — autosave the basket: replaces the whole item
// list of the caller's draft. The update contract carries items only
// (currency is fixed at draft creation), which is why the wizard locks the
// currency once a draft exists. 403 (foreign draft) and 409 (no longer
// draft) pass through so the client can stop autosaving instead of
// retry-looping.
export async function PUT(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const body = (await request.json().catch(() => null)) as { items?: RequisitionItemWire[] } | null;
  if (!body || !Array.isArray(body.items)) {
    return NextResponse.json({ error: "items must be an array", code: "body_invalid" }, { status: 400 });
  }
  const result = await replaceRequisitionItems(tokenOr401, id, body.items);
  return forwardGatewayResult(result);
}
