// match-result tests (issue #41): the #40 engine's pinned JSON shape —
// parse/Δ derivation and the reason-line parsing the panel localizes with.
import { describe, expect, it } from "vitest";
import { fill, parseMatchResult, parseReason } from "./match-result";

const matchedJSON = JSON.stringify({
  matched: true,
  lines: [
    {
      po_item_id: "po-item-1",
      qty_ordered: 10,
      qty_received: 10,
      qty_invoiced: 10,
      po_unit_price_cents: 100000,
      invoice_unit_price_cents: 100000,
      qty_breach: false,
      price_breach: false,
      reason: "",
    },
  ],
});

const discrepancyJSON = JSON.stringify({
  matched: false,
  lines: [
    {
      po_item_id: "po-item-2",
      qty_ordered: 50,
      qty_received: 60,
      qty_invoiced: 80,
      po_unit_price_cents: 100000,
      invoice_unit_price_cents: 115000,
      qty_breach: true,
      price_breach: true,
      reason: "inv_over_bill_qty: po_item=po-item-2 invoiced=80 received=60 pct=0",
    },
    {
      po_item_id: "po-item-3",
      qty_ordered: 5,
      qty_received: 5,
      qty_invoiced: 5,
      po_unit_price_cents: 200000,
      invoice_unit_price_cents: 150000,
      qty_breach: false,
      price_breach: false,
      reason: "",
    },
  ],
});

describe("parseMatchResult", () => {
  it("returns null for empty, malformed, or shape-mismatched input", () => {
    expect(parseMatchResult("")).toBeNull();
    expect(parseMatchResult("not json")).toBeNull();
    expect(parseMatchResult("42")).toBeNull();
    expect(parseMatchResult('{"matched":true}')).toBeNull(); // no lines array
    expect(
      parseMatchResult('{"matched":true,"lines":[{"po_item_id":"a"}]}'), // missing fields
    ).toBeNull();
  });

  it("parses a matched result with clean-line zeros for both deltas", () => {
    const result = parseMatchResult(matchedJSON);
    expect(result).not.toBeNull();
    expect(result?.matched).toBe(true);
    expect(result?.lines).toHaveLength(1);
    const line = result?.lines[0];
    expect(line?.qtyDelta).toBe(0);
    expect(line?.priceDeltaCents).toBe(0);
  });

  it("derives the qty delta (invoiced − received) and the price delta per line", () => {
    const result = parseMatchResult(discrepancyJSON);
    expect(result?.matched).toBe(false);
    const [breach, clean] = result?.lines ?? [];
    // 80 invoiced vs 60 received: +20 over the receipts; price +15,000 over PO.
    expect(breach.qtyDelta).toBe(20);
    expect(breach.priceDeltaCents).toBe(15000);
    expect(breach.qty_breach).toBe(true);
    expect(breach.price_breach).toBe(true);
    // Under-billed price is a negative delta (progress billing: −50,000).
    expect(clean.qtyDelta).toBe(0);
    expect(clean.priceDeltaCents).toBe(-50000);
    expect(clean.qty_breach).toBe(false);
    expect(clean.price_breach).toBe(false);
  });

  it("carries the raw reason lines through untouched", () => {
    const result = parseMatchResult(discrepancyJSON);
    expect(result?.lines[0].reason).toBe("inv_over_bill_qty: po_item=po-item-2 invoiced=80 received=60 pct=0");
    expect(result?.lines[1].reason).toBe("");
  });
});

describe("parseReason", () => {
  it("parses the pinned qty reason with its numbers", () => {
    expect(parseReason("inv_over_bill_qty: po_item=po-item-2 invoiced=80 received=60 pct=10")).toEqual({
      code: "inv_over_bill_qty",
      poItem: "po-item-2",
      invoiced: 80,
      received: 60,
      pct: 10,
    });
  });

  it("parses the pinned price reason with its numbers", () => {
    expect(parseReason("inv_over_bill_price: po_item=abc po_price=100000 invoice_price=115000 pct=0")).toEqual({
      code: "inv_over_bill_price",
      poItem: "abc",
      poPrice: 100000,
      invoicePrice: 115000,
      pct: 0,
    });
  });

  it("returns null for the clean-line reason and unrecognized shapes", () => {
    expect(parseReason("")).toBeNull();
    expect(parseReason("something else entirely")).toBeNull();
    expect(parseReason("inv_over_bill_qty: invoiced=1 received=1 pct=0")).toBeNull(); // missing po_item
  });
});

describe("fill", () => {
  it("interpolates {placeholders} with the parsed numbers", () => {
    expect(fill("billed {invoiced} vs received {received} ({pct}%)", { invoiced: 80, received: 60, pct: 5 })).toBe(
      "billed 80 vs received 60 (5%)",
    );
    expect(fill("no numbers")).toBe("no numbers");
    expect(fill("unknown {missing} stays", {})).toBe("unknown {missing} stays");
  });
});
