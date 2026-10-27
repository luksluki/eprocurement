import { NextResponse } from "next/server";
import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { decideApprovalStep } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/approvals/[stepId]/decide — BFF proxy for the requisition detail
// page's approve/reject actions (issue #34; gateway contract issue #31). The
// access token stays in the httpOnly cookie; the client only ever talks to
// this route. The optional Idempotency-Key header (1–64 chars after
// trimming, same convention as the submit route) names the decision so a
// double-click can never record two. Gateway 4xx bodies ride through
// verbatim (409 approval_step_not_current, 403 approval_self_forbidden /
// approval_role_forbidden, …) so the UI can map machine codes to messages;
// 401 stays the refresh-dance signal; anything else becomes 502.
export async function POST(request: Request, { params }: { params: Promise<{ stepId: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { stepId } = await params;
  const body = (await request.json().catch(() => null)) as { decision?: string; reason?: string } | null;
  if (!body || (body.decision !== "approve" && body.decision !== "reject")) {
    return NextResponse.json({ error: "decision must be approve or reject", code: "approval_decision_invalid" }, { status: 400 });
  }
  const key = request.headers.get("idempotency-key") ?? undefined;
  if (key !== undefined && (key.trim().length < 1 || key.trim().length > 64)) {
    return NextResponse.json(
      { error: "Idempotency-Key must be 1-64 characters", code: "approval_idempotency_key_invalid" },
      { status: 400 },
    );
  }
  const result = await decideApprovalStep(tokenOr401, stepId, body.decision, body.reason ?? "", key?.trim() || undefined);
  return forwardGatewayResult(result);
}
