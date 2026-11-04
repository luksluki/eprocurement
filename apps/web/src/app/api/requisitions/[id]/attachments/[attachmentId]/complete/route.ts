import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { completeAttachment } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/v1/requisitions/[id]/attachments/[attachmentId]/complete —
// verify the uploaded object and flip the attachment to ready (#33).
// Verification failures (attachment_object_missing, attachment_size_mismatch,
// attachment_mime_mismatch, attachment_checksum_mismatch) are 412 with the
// machine code and ride through verbatim so the UI can show which file
// failed and why.
export async function POST(
  _request: Request,
  { params }: { params: Promise<{ id: string; attachmentId: string }> },
) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id, attachmentId } = await params;
  const result = await completeAttachment(tokenOr401, id, attachmentId);
  return forwardGatewayResult(result);
}
