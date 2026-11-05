import { NextResponse } from "next/server";
import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { createPurchaseOrderFromRequisition } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// The edge's po_vendor_id_invalid mirror (#35): procurement parses vendor_id
// with uuid.Parse and the gateway trims it, so an empty or non-UUID value is
// rejected before any transaction opens. The BFF re-checks locally so garbage
// never reaches the gateway.
const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

// POST /api/purchase-orders/from-requisition/[id] — BFF proxy for the
// requisition detail page's convert action (issue #112; gateway contract
// #35). The access token stays in the httpOnly cookie; the client only ever
// talks to this route. The body is {vendor_id} (the requisition carries no
// vendor, PRD §13): malformed JSON answers 400 body_invalid and a missing or
// non-UUID vendor_id answers 400 po_vendor_id_invalid — both locally,
// without a gateway call (the goods-receipts mirror pattern). The caller's
// X-Forwarded-For is forwarded so the po.created audit row records the real
// client IP, not the BFF's address (#44). The success status mirrors the
// gateway's: 201 = this call created the draft PO, 200 = idempotent replay
// of the existing PO (never an error). Gateway 4xx bodies ride through
// verbatim so the UI can map machine codes to messages; 401 stays the
// refresh-dance signal; anything else becomes 502.
export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const body = (await request.json().catch(() => null)) as { vendor_id?: string } | null;
  if (!body) {
    return NextResponse.json({ error: "invalid JSON body", code: "body_invalid" }, { status: 400 });
  }
  const vendorId = typeof body.vendor_id === "string" ? body.vendor_id.trim() : "";
  if (vendorId === "" || !UUID_RE.test(vendorId)) {
    return NextResponse.json(
      { error: "vendor_id must be the UUID of the vendor to order from", code: "po_vendor_id_invalid" },
      { status: 400 },
    );
  }
  const result = await createPurchaseOrderFromRequisition(
    tokenOr401,
    id,
    vendorId,
    request.headers.get("x-forwarded-for") ?? undefined,
  );
  return forwardGatewayResult(result, 201);
}
