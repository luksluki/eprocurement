import { NextResponse } from "next/server";
import { accessTokenOr401, forwardGatewayResult, isNextResponse } from "@/lib/bff";
import { createInvoice } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// POST /api/invoices — BFF proxy for the create-invoice page (issue #41;
// gateway contract issue #39). The access token stays in the httpOnly
// cookie; the client only ever talks to this route. Mirrors the #39 request
// shape (non-empty items, integer qty > 0, price >= 0) so a malformed body
// answers 400 body_invalid before any gateway call; gateway 4xx bodies ride
// through verbatim (409 inv_over_bill with the pinned detail line, 409
// inv_number_duplicate, 409 po_illegal_transition, …) so the form can map
// machine codes to messages; 401 stays the refresh-dance signal.
export async function POST(request: Request) {
  const tokenOr401 = await accessTokenOr401();
  if (isNextResponse(tokenOr401)) {
    return tokenOr401;
  }
  const body = (await request.json().catch(() => null)) as {
    po_id?: string;
    number?: string;
    items?: { po_item_id?: string; qty?: number; unit_price_cents?: number }[];
  } | null;
  if (
    !body ||
    typeof body.po_id !== "string" ||
    typeof body.number !== "string" ||
    !Array.isArray(body.items) ||
    body.items.length === 0 ||
    body.items.some(
      (it) =>
        typeof it.po_item_id !== "string" ||
        !Number.isInteger(it.qty) ||
        (it.qty ?? 0) <= 0 ||
        typeof it.unit_price_cents !== "number" ||
        !Number.isInteger(it.unit_price_cents) ||
        it.unit_price_cents < 0,
    )
  ) {
    return NextResponse.json(
      { error: "po_id, number, and items (po_item_id, integer qty > 0, unit_price_cents >= 0) are required", code: "body_invalid" },
      { status: 400 },
    );
  }
  const result = await createInvoice(
    tokenOr401,
    body.po_id,
    body.number,
    body.items.map((it) => ({
      po_item_id: it.po_item_id ?? "",
      qty: it.qty ?? 0,
      unit_price_cents: it.unit_price_cents ?? 0,
    })),
  );
  return forwardGatewayResult(result, 201);
}
