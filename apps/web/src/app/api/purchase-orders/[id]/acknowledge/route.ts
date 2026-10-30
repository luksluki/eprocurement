import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { acknowledgePurchaseOrder } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/purchase-orders/[id]/acknowledge — BFF proxy for the PO detail
// page's acknowledge action (issue #38; gateway contract issue #36). Same
// conventions as the issue route: no body, gateway 4xx verbatim, 401 is the
// refresh-dance signal, 502 otherwise. The caller's X-Forwarded-For is
// forwarded for the event envelope's actor_ip (#44).
export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const result = await acknowledgePurchaseOrder(tokenOr401, id, request.headers.get("x-forwarded-for") ?? undefined);
  return forwardGatewayResult(result);
}
