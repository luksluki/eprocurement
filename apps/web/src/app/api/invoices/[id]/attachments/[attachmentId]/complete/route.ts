import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { completeInvoiceAttachment } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/invoices/[id]/attachments/[attachmentId]/complete — tell the
// gateway to verify the uploaded object (size/mime/checksum) and flip the
// attachment to ready (issue #41, the #33 requisition pattern). Ready state
// unlocks the Match button's hard gate.
export async function POST(
  _request: Request,
  { params }: { params: Promise<{ id: string; attachmentId: string }> },
) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id, attachmentId } = await params;
  const result = await completeInvoiceAttachment(tokenOr401, id, attachmentId);
  return forwardGatewayResult(result);
}
