// MatchResultPanel tests (issue #41): the pending state, the verdict badge,
// the per-line diff (ordered/received/invoiced + Δ qty; PO vs invoice price
// + Δ), the breach badges, and the localized reasons with the parsed
// numbers — the "cause of the mismatch on one screen" AC.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import MatchResultPanel from "./MatchResultPanel";

const copy = {
  heading: "Three-way match result",
  verdictMatched: "Matched",
  verdictDiscrepancy: "Discrepancy",
  pending: "Not run yet — run the match to fill in the result.",
  invalid: "The stored match result could not be read.",
  columns: {
    poItem: "PO line",
    qtyOrdered: "Ordered",
    qtyReceived: "Received",
    qtyInvoiced: "Invoiced",
    qtyDelta: "Δ qty",
    poPrice: "PO price",
    invoicePrice: "Invoice price",
    priceDelta: "Δ price",
    flags: "Flags",
    reason: "Reason",
  },
  qtyBreach: "qty",
  priceBreach: "price",
  ok: "ok",
  reasonQty: "Quantity over-billed: {invoiced} invoiced while only {received} were received (tolerance {pct}%).",
  reasonPrice: "Price differs: the PO price is {poPrice} but the invoice bills {invoicePrice} (tolerance {pct}%).",
};

function renderPanel(json: string) {
  return render(
    <MatchResultPanel matchResultJson={json} currency="IDR" localeTag="id-ID" copy={copy} />,
  );
}

afterEach(cleanup);

describe("MatchResultPanel", () => {
  it("renders the pending state for an empty result (never matched)", () => {
    renderPanel("");
    expect(screen.getByTestId("match-result-pending").textContent).toContain(copy.pending);
    expect(screen.queryByTestId("match-result-panel")).toBeNull();
  });

  it("renders the invalid state for a malformed payload instead of crashing", () => {
    renderPanel("{not json");
    expect(screen.getByTestId("match-result-pending").textContent).toContain(copy.invalid);
  });

  it("renders the matched verdict with a clean line (Δ 0 / Δ Rp 0)", () => {
    renderPanel(
      JSON.stringify({
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
      }),
    );
    expect(screen.getByTestId("match-verdict").textContent).toBe("Matched");
    expect(screen.getByTestId("match-qty-delta-po-item-1").textContent).toBe("0");
    expect(screen.getByTestId("match-price-delta-po-item-1").textContent).toContain("0");
    expect(screen.getByTestId("match-reason-po-item-1").textContent).toBe("—");
    expect(screen.getByTestId("match-lines").textContent).toContain(copy.ok);
  });

  it("shows the discrepancy verdict, both deltas, breach badges, and the localized qty reason with parsed numbers", () => {
    renderPanel(
      JSON.stringify({
        matched: false,
        lines: [
          {
            po_item_id: "po-item-2",
            qty_ordered: 50,
            qty_received: 60,
            qty_invoiced: 80,
            po_unit_price_cents: 100000,
            invoice_unit_price_cents: 100000,
            qty_breach: true,
            price_breach: false,
            reason: "inv_over_bill_qty: po_item=po-item-2 invoiced=80 received=60 pct=10",
          },
        ],
      }),
    );
    expect(screen.getByTestId("match-verdict").textContent).toBe("Discrepancy");
    // Δ qty +80−60 = +20 (invoiced over received), rendered signed.
    expect(screen.getByTestId("match-qty-delta-po-item-2").textContent).toBe("+20");
    expect(screen.getByTestId("match-lines").textContent).toContain(copy.qtyBreach);
    expect(screen.queryByTestId("match-reason-po-item-2")?.textContent).toBe(
      "Quantity over-billed: 80 invoiced while only 60 were received (tolerance 10%).",
    );
  });

  it("localizes the price reason with money-formatted parsed numbers and the price delta in money", () => {
    renderPanel(
      JSON.stringify({
        matched: false,
        lines: [
          {
            po_item_id: "po-item-3",
            qty_ordered: 5,
            qty_received: 5,
            qty_invoiced: 5,
            po_unit_price_cents: 100000,
            invoice_unit_price_cents: 150000,
            qty_breach: false,
            price_breach: true,
            reason: "inv_over_bill_price: po_item=po-item-3 po_price=100000 invoice_price=150000 pct=0",
          },
        ],
      }),
    );
    // Δ price is cents: 150.000 − 100.000 = 50.000 cents = Rp 500, signed.
    expect(screen.getByTestId("match-price-delta-po-item-3").textContent?.replace(/\u00A0/g, " ")).toBe("+Rp 500");
    // The reason's amounts are money-formatted (cents / 100).
    expect(screen.getByTestId("match-reason-po-item-3").textContent?.replace(/\u00A0/g, " ")).toBe(
      "Price differs: the PO price is Rp 1.000 but the invoice bills Rp 1.500 (tolerance 0%).",
    );
    expect(screen.getByTestId("match-lines").textContent).toContain(copy.priceBreach);
  });
});
