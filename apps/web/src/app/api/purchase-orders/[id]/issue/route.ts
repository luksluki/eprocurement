import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { issuePurchaseOrder } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/purchase-orders/[id]/issue — BFF proxy for the PO detail page's
// issue action (issue #38; gateway contract issue #36). The access token
// stays in the httpOnly cookie; the client only ever talks to this route. No
// request body (the path id names the PO). The caller's X-Forwarded-For is
// forwarded so the po.issued audit row records the real client IP, not the
// BFF's address (#44). Gateway 4xx bodies ride through verbatim (409
// po_illegal_transition, 404 po_not_found, …) so the UI can map machine
// codes to messages; 401 stays the refresh-dance signal.
export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const result = await issuePurchaseOrder(tokenOr401, id, request.headers.get("x-forwarded-for") ?? undefined);
  return forwardGatewayResult(result);
}
