import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { matchInvoice } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/invoices/[id]/match — BFF proxy for the invoice detail page's
// Match action (issue #41; gateway contract issue #40). The access token
// stays in the httpOnly cookie; no request body (the path id names the
// invoice). Gateway 4xx bodies ride through verbatim (409
// inv_already_matched, 404 inv_not_found, …) so the actions can map machine
// codes to messages; 401 stays the refresh-dance signal.
export async function POST(_request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const result = await matchInvoice(tokenOr401, id);
  return forwardGatewayResult(result);
}
