// Match-result parsing for the invoice detail's MatchResultPanel (issue
// #41): renders the #40 engine's match_result_json — the verdict plus the
// per-line numbers and the pinned reason lines — so a finance user can see
// the cause of a mismatch on one screen (the issue's AC).
//
// The wire shape is pinned by services/procurement/match (Result.ResultJSON,
// dev-note 040): {"matched":bool,"lines":[{po_item_id, qty_ordered,
// qty_received, qty_invoiced, po_unit_price_cents,
// invoice_unit_price_cents, qty_breach, price_breach, reason}]}. The reason
// lines are the inv_over_bill family, machine code before the first colon
// then numbers:
//   inv_over_bill_qty: po_item=<id> invoiced=<i> received=<r> pct=<p>
//   inv_over_bill_price: po_item=<id> po_price=<p> invoice_price=<q> pct=<p>

export type MatchResultLine = {
  po_item_id: string;
  qty_ordered: number;
  qty_received: number;
  qty_invoiced: number;
  po_unit_price_cents: number;
  invoice_unit_price_cents: number;
  qty_breach: boolean;
  price_breach: boolean;
  reason: string;
  /** qty_invoiced − qty_received: how far the cumulative billing exceeds the
   * receipts (negative = progress billing, never a breach). */
  qtyDelta: number;
  /** invoice − PO unit price, in cents (negative = under-billed). */
  priceDeltaCents: number;
};

export type MatchResult = {
  matched: boolean;
  lines: MatchResultLine[];
};

/**
 * parseMatchResult decodes the stored match_result_json and derives the
 * deltas the panel shows. Returns null for empty (never matched), malformed,
 * or shape-mismatched input — the panel renders its "no result yet" state
 * instead of crashing.
 */
export function parseMatchResult(json: string): MatchResult | null {
  if (!json) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(json);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const root = parsed as { matched?: unknown; lines?: unknown };
  if (typeof root.matched !== "boolean" || !Array.isArray(root.lines)) return null;
  const lines: MatchResultLine[] = [];
  for (const entry of root.lines) {
    if (typeof entry !== "object" || entry === null) return null;
    const l = entry as Record<string, unknown>;
    if (
      typeof l.po_item_id !== "string" ||
      typeof l.qty_ordered !== "number" ||
      typeof l.qty_received !== "number" ||
      typeof l.qty_invoiced !== "number" ||
      typeof l.po_unit_price_cents !== "number" ||
      typeof l.invoice_unit_price_cents !== "number" ||
      typeof l.qty_breach !== "boolean" ||
      typeof l.price_breach !== "boolean" ||
      typeof l.reason !== "string"
    ) {
      return null;
    }
    lines.push({
      po_item_id: l.po_item_id,
      qty_ordered: l.qty_ordered,
      qty_received: l.qty_received,
      qty_invoiced: l.qty_invoiced,
      po_unit_price_cents: l.po_unit_price_cents,
      invoice_unit_price_cents: l.invoice_unit_price_cents,
      qty_breach: l.qty_breach,
      price_breach: l.price_breach,
      reason: l.reason,
      qtyDelta: l.qty_invoiced - l.qty_received,
      priceDeltaCents: l.invoice_unit_price_cents - l.po_unit_price_cents,
    });
  }
  return { matched: root.matched, lines };
}

/** The parsed numbers of one pinned reason line, keyed by its machine code. */
export type ParsedReason =
  | { code: "inv_over_bill_qty"; poItem: string; invoiced: number; received: number; pct: number }
  | { code: "inv_over_bill_price"; poItem: string; poPrice: number; invoicePrice: number; pct: number };

/**
 * parseReason extracts the machine code, the po_item, and the numbers from
 * one pinned reason line (see the format above). Returns null for the empty
 * reason (a clean line) or an unrecognized shape — callers fall back to the
 * raw string.
 */
export function parseReason(reason: string): ParsedReason | null {
  if (!reason) return null;
  const qty = /^inv_over_bill_qty: po_item=(\S+) invoiced=(\d+) received=(\d+) pct=(\d+)$/.exec(reason);
  if (qty) {
    return {
      code: "inv_over_bill_qty",
      poItem: qty[1],
      invoiced: Number(qty[2]),
      received: Number(qty[3]),
      pct: Number(qty[4]),
    };
  }
  const price =
    /^inv_over_bill_price: po_item=(\S+) po_price=(\d+) invoice_price=(\d+) pct=(\d+)$/.exec(reason);
  if (price) {
    return {
      code: "inv_over_bill_price",
      poItem: price[1],
      poPrice: Number(price[2]),
      invoicePrice: Number(price[3]),
      pct: Number(price[4]),
    };
  }
  return null;
}

/** fill interpolates {placeholder} templates — the dictionary copy crosses
 * the server→client boundary as plain strings (functions cannot). */
export function fill(template: string, values: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (_, key: string) => (key in values ? String(values[key]) : `{${key}}`));
}
