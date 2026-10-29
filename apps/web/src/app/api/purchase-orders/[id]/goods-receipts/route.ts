import { NextResponse } from "next/server";
import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { postGoodsReceipt } from "@/lib/gateway";

export const dynamic = "force-dynamic";

const MAX_NOTES_LEN = 1000;

// POST /api/purchase-orders/[id]/goods-receipts — BFF proxy for the PO
// detail page's goods-receipt form (issue #38; gateway contract issue #37).
// The access token stays in the httpOnly cookie. The request shape is
// re-validated here as a mirror of the edge (non-empty lines, whole qty > 0,
// unique po_item_ids, notes ≤ 1000) so garbage never reaches the gateway;
// gateway 4xx bodies ride through verbatim — a 409 grn_over_receipt carries
// the pinned detail line in `error`, which the form parses for its inline
// numbers. 401 stays the refresh-dance signal; anything else becomes 502.
export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const { id } = await params;
  const body = (await request.json().catch(() => null)) as
    | { items?: { po_item_id?: string; qty_received?: number }[]; notes?: string }
    | null;
  if (!body || !Array.isArray(body.items) || body.items.length === 0) {
    return NextResponse.json(
      { error: "a goods receipt needs at least one item line", code: "grn_items_empty" },
      { status: 400 },
    );
  }
  const seen = new Set<string>();
  for (const it of body.items) {
    const poItemId = typeof it?.po_item_id === "string" ? it.po_item_id.trim() : "";
    if (poItemId === "" || !Number.isInteger(it?.qty_received) || (it?.qty_received ?? 0) <= 0) {
      return NextResponse.json(
        { error: "each received quantity must be a whole number greater than zero", code: "grn_qty_invalid" },
        { status: 400 },
      );
    }
    if (seen.has(poItemId)) {
      return NextResponse.json(
        { error: "each purchase order line may appear once per goods receipt", code: "grn_po_item_duplicate" },
        { status: 400 },
      );
    }
    seen.add(poItemId);
  }
  const notes = typeof body.notes === "string" ? body.notes.trim() : "";
  if (notes.length > MAX_NOTES_LEN) {
    return NextResponse.json({ error: "notes are limited to 1000 characters", code: "grn_notes_invalid" }, { status: 400 });
  }
  const items = body.items.map((it) => ({
    po_item_id: (it.po_item_id ?? "").trim(),
    qty_received: it.qty_received as number,
  }));
  const result = await postGoodsReceipt(tokenOr401, id, items, notes);
  return forwardGatewayResult(result, 201);
}
