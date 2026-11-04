import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { submitRequisition } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/requisitions/[id]/submit — final wizard action (#33). The
// optional Idempotency-Key (1-64 chars, forwarded as the header) names the
// submit: a replay returns the requisition's current state. Gateway 4xx
// bodies ride through verbatim (409 requisition_illegal_transition, 412
// approval_rules_missing, 400 requisition_items_empty, …) for per-step
// mapping in the UI.
export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const key = request.headers.get("idempotency-key") ?? undefined;
  if (key !== undefined && (key.trim().length < 1 || key.trim().length > 64)) {
    return Response.json({ error: "Idempotency-Key must be 1-64 characters", code: "requisition_idempotency_key_invalid" }, { status: 400 });
  }
  const result = await submitRequisition(tokenOr401, id, key?.trim() || undefined);
  return forwardGatewayResult(result);
}
