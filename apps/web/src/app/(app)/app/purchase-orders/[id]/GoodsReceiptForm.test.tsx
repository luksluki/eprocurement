// GoodsReceiptForm tests (issue #38): per-field client validation without a
// round trip (qty whole > 0, never above the remaining, at least one filled
// line, notes bound), the pinned over-receipt 409 renders inline with its
// exact numbers, a success shows the GRN number and refreshes the server
// component, and a 401 runs the refresh dance.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import GoodsReceiptForm, { fill, parseOverReceipt } from "./GoodsReceiptForm";

const mocks = vi.hoisted(() => ({
  refresh: vi.fn(),
  assign: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: mocks.refresh }),
  usePathname: () => "/app/purchase-orders/po-1",
}));

const copy = {
  heading: "Record a goods receipt (GRN)",
  hint: "Enter what actually arrived per line.",
  qtyLabel: "Received now",
  notesLabel: "Notes",
  notesPlaceholder: "Receiving notes…",
  submit: "Record receipt",
  submitting: "Recording…",
  qtyInvalid: "Quantity must be a whole number greater than 0.",
  qtyOver: "Quantity exceeds this line's remaining {remaining}.",
  minOneLine: "Fill at least one line with a quantity greater than 0.",
  notesTooLong: "Notes are limited to 1000 characters.",
  successTitle: "Receipt recorded",
  sessionExpired: "Session expired — redirecting to sign-in…",
  genericError: "Failed to record the receipt — try again.",
  errorMessages: {
    po_illegal_transition: "Goods can only be received on an acknowledged or partially received purchase order.",
    grn_qty_invalid: "Each received quantity must be greater than zero.",
  },
  overReceipt:
    "Over-receipt: already received {received}, requests {requested} more, only {ordered} ordered.",
};

const items = [
  { id: "it-1", description: "Laptop", qty_ordered: 2, qty_received: 0, unit_price_cents: 7500000 },
  { id: "it-2", description: "Mouse", qty_ordered: 3, qty_received: 1, unit_price_cents: 150000 },
  { id: "it-3", description: "Dock", qty_ordered: 1, qty_received: 1, unit_price_cents: 900000 },
];

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

const originalLocation = window.location;

beforeEach(() => {
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...originalLocation, assign: mocks.assign },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  mocks.refresh.mockClear();
  mocks.assign.mockClear();
  Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
});

describe("parseOverReceipt", () => {
  it("extracts the pinned detail numbers from the gateway error string", () => {
    const detail = "grn_over_receipt: po_item=11111111-0000-0000-0000-000000000001 received=1 requested=2 ordered=2";
    expect(parseOverReceipt(detail)).toEqual({ received: 1, requested: 2, ordered: 2 });
  });

  it("returns null when the shape does not match", () => {
    expect(parseOverReceipt("something else")).toBeNull();
  });
});

describe("GoodsReceiptForm", () => {
  it("renders one input per line; a fully received line is disabled", () => {
    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    expect((screen.getByTestId("grn-qty-it-1") as HTMLInputElement).disabled).toBe(false); // remaining 2
    expect((screen.getByTestId("grn-qty-it-2") as HTMLInputElement).disabled).toBe(false); // remaining 2
    expect((screen.getByTestId("grn-qty-it-3") as HTMLInputElement).disabled).toBe(true); // remaining 0
  });

  it("rejects an empty form locally: no network call, min-one-line message", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.click(screen.getByTestId("grn-submit"));

    const error = await screen.findByTestId("grn-form-error");
    expect(error.textContent).toBe(copy.minOneLine);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("validates per field without a round trip: zero and over-remaining qty", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "0" } });
    fireEvent.change(screen.getByTestId("grn-qty-it-2"), { target: { value: "3" } }); // remaining is 2
    fireEvent.click(screen.getByTestId("grn-submit"));

    expect(await screen.findByTestId("grn-error-it-1")).toBeTruthy();
    expect(screen.getByTestId("grn-error-it-1").textContent).toBe(copy.qtyInvalid);
    expect(screen.getByTestId("grn-error-it-2").textContent).toBe(fill(copy.qtyOver, { remaining: 2 }));
    expect(screen.getByTestId("grn-form-error").textContent).toBe(copy.minOneLine);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects fractional quantities locally", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "1.5" } });
    fireEvent.click(screen.getByTestId("grn-submit"));

    expect(await screen.findByTestId("grn-error-it-1")).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects over-long notes locally", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "1" } });
    fireEvent.change(screen.getByTestId("grn-notes"), { target: { value: "x".repeat(1001) } });
    fireEvent.click(screen.getByTestId("grn-submit"));

    const error = await screen.findByTestId("grn-form-error");
    expect(error.textContent).toBe(copy.notesTooLong);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("POSTs the filled lines and trimmed notes; success shows the GRN number and refreshes", async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse({ id: "grn-1", number: "GRN-000001", po_id: "po-1", items: [], purchase_order: { id: "po-1" } }, 201),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "2" } });
    fireEvent.change(screen.getByTestId("grn-qty-it-2"), { target: { value: "2" } });
    fireEvent.change(screen.getByTestId("grn-notes"), { target: { value: "  dock 3  " } });
    fireEvent.click(screen.getByTestId("grn-submit"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/purchase-orders/po-1/goods-receipts");
    expect(init.method).toBe("POST");
    // Only the filled lines ride along, in item order.
    expect(JSON.parse(String(init.body))).toEqual({
      items: [
        { po_item_id: "it-1", qty_received: 2 },
        { po_item_id: "it-2", qty_received: 2 },
      ],
      notes: "dock 3",
    });
    const confirmation = await screen.findByTestId("grn-confirmation");
    expect(confirmation.textContent).toContain("GRN-000001");
  });

  it("renders the over-receipt 409 inline with the pinned numbers", async () => {
    const detail = "grn_over_receipt: po_item=it-1 received=1 requested=2 ordered=2";
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ error: detail, code: "grn_over_receipt" }, 409)),
    );

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "2" } });
    fireEvent.click(screen.getByTestId("grn-submit"));

    const error = await screen.findByTestId("grn-form-error");
    expect(error.textContent).toBe(fill(copy.overReceipt, { received: 1, requested: 2, ordered: 2 }));
    expect(mocks.refresh).not.toHaveBeenCalled();
  });

  it("maps other machine codes to localized messages (po_illegal_transition)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ error: "not acknowledged", code: "po_illegal_transition" }, 409)),
    );

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("grn-submit"));

    const error = await screen.findByTestId("grn-form-error");
    expect(error.textContent).toBe(copy.errorMessages.po_illegal_transition);
  });

  it("runs the refresh dance on 401 with the session-expired notice", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Session expired." }, 401)));

    render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("grn-submit"));

    await waitFor(() =>
      expect(mocks.assign).toHaveBeenCalledWith("/api/auth/refresh?next=%2Fapp%2Fpurchase-orders%2Fpo-1"),
    );
    expect((await screen.findByTestId("grn-form-error")).textContent).toBe(copy.sessionExpired);
  });

  it("resets the per-line state when fresh items arrive (router.refresh after a receipt)", async () => {
    // A partial receipt lands: it-1 goes 2/2 (was 0/2). The success path
    // calls router.refresh(); the server component re-renders with the
    // advanced quantities as new props — the stale-snapshot bug the live
    // proof caught kept the old remainders (and a leftover quantity).
    const fetchMock = vi.fn(async () =>
      jsonResponse({ id: "grn-1", number: "GRN-000002", po_id: "po-1", items: [], purchase_order: { id: "po-1" } }, 201),
    );
    vi.stubGlobal("fetch", fetchMock);

    const view = render(<GoodsReceiptForm poId="po-1" items={items} copy={copy} />);
    fireEvent.change(screen.getByTestId("grn-qty-it-1"), { target: { value: "2" } });
    fireEvent.change(screen.getByTestId("grn-qty-it-2"), { target: { value: "2" } });
    fireEvent.click(screen.getByTestId("grn-submit"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    // The refreshed aggregate: it-1 fully received (2/2), it-2 advanced 2/3.
    const refreshed = items.map((it) => ({ ...it }));
    refreshed[0] = { ...refreshed[0], qty_received: 2 };
    refreshed[1] = { ...refreshed[1], qty_received: 2 };
    view.rerender(<GoodsReceiptForm poId="po-1" items={refreshed} copy={copy} />);

    // Inputs reset to empty; the full line is now disabled; the GRN number
    // confirmation stays visible.
    expect((screen.getByTestId("grn-qty-it-1") as HTMLInputElement).value).toBe("");
    expect((screen.getByTestId("grn-qty-it-1") as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByTestId("grn-qty-it-2") as HTMLInputElement).value).toBe("");
    expect((screen.getByTestId("grn-qty-it-2") as HTMLInputElement).disabled).toBe(false);
    expect(screen.getByTestId("grn-confirmation").textContent).toContain("GRN-000002");
  });
});
