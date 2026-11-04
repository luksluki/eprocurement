import { NextResponse } from "next/server";
import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { declareInvoiceAttachment } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/invoices/[id]/attachments — declare an upload (issue #41, the
// #33 requisition pattern): the gateway checks the invoice exists and is the
// caller's, then documents returns the presigned PUT URL. Machine codes from
// either service (inv_not_found, attachment_too_large, attachment_mime_invalid,
// …) ride through verbatim for the panel's mapping.
export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const body = (await request.json().catch(() => null)) as {
    filename?: string;
    mime_type?: string;
    size_bytes?: number;
    checksum_sha256?: string;
  } | null;
  if (
    !body ||
    typeof body.filename !== "string" ||
    typeof body.mime_type !== "string" ||
    typeof body.size_bytes !== "number" ||
    typeof body.checksum_sha256 !== "string"
  ) {
    return NextResponse.json(
      { error: "filename, mime_type, size_bytes, and checksum_sha256 are required", code: "body_invalid" },
      { status: 400 },
    );
  }
  const result = await declareInvoiceAttachment(tokenOr401, id, {
    filename: body.filename,
    mime_type: body.mime_type,
    size_bytes: body.size_bytes,
    checksum_sha256: body.checksum_sha256,
  });
  return forwardGatewayResult(result, 201);
}
