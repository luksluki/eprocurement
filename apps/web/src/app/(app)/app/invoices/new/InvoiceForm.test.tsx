// InvoiceForm tests (issue #41): per-field validation mirrors the #39 domain
// rules without a network call, prices prefill with the PO price, a valid
// submit POSTs the parsed lines and redirects to the new detail, the pinned
// 409 inv_over_bill numbers render inline, and a 401 runs the refresh dance.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { PurchaseOrderItem } from "@/lib/gateway";
import InvoiceForm, { parseOverBill } from "./InvoiceForm";

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  assign: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: mocks.push }),
  usePathname: () => "/app/invoices/new?po=po-1",
}));

const copy = {
  heading: "Invoice for PO",
  numberLabel: "Vendor invoice number",
  numberPlaceholder: "INV/2026/0001",
  itemsHeading: "Lines to bill",
  itemsHint: "Fill the quantity for each line you bill.",
  itemsLine: "PO line",
  itemsQty: "Qty",
  itemsUnitPrice: "Unit price (cents)",
  itemsLineTotal: "Line total",
  submit: "Save invoice",
  submitting: "Saving…",
  numberInvalid: "The invoice number must be 1–100 characters after trimming.",
  qtyInvalid: "Quantity must be a whole number greater than 0.",
  priceInvalid: "Unit price must be zero or more.",
  minOneLine: "Fill at least one line with a quantity greater than 0.",
  genericError: "Failed to save the invoice — try again.",
  sessionExpired: "Session expired — redirecting to sign-in…",
  errorMessages: {
    inv_number_duplicate: "This vendor already has an invoice with this number.",
    po_illegal_transition: "Only acknowledged, partially received, or received POs can be invoiced.",
  },
  overBill:
    "Over-bills PO {poNumber}: {invoiced} already invoiced and this invoice requests {requested} more, but the total is {total}.",
};

const items: PurchaseOrderItem[] = [
  { id: "po-item-1", description: "Laptop", qty_ordered: 2, qty_received: 2, unit_price_cents: 7500000 },
  { id: "po-item-2", description: "Mouse", qty_ordered: 3, qty_received: 3, unit_price_cents: 150000 },
];

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

const originalLocation = window.location;

function renderForm() {
  return render(
    <InvoiceForm poId="po-1" poNumber="PO-000001" currency="IDR" localeTag="id-ID" items={items} copy={copy} />,
  );
}

beforeEach(() => {
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...originalLocation, assign: mocks.assign },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  mocks.push.mockClear();
  mocks.assign.mockClear();
  Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
});

describe("InvoiceForm", () => {
  it("prefills every unit price with the PO price", () => {
    renderForm();
    expect((screen.getByTestId("invoice-price-po-item-1") as HTMLInputElement).value).toBe("7500000");
    expect((screen.getByTestId("invoice-price-po-item-2") as HTMLInputElement).value).toBe("150000");
  });

  it("validates per-field without a network call: empty number, bad qty, bad price, no lines", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    renderForm();
    // Bad qty (0) on line 1, nothing else filled.
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-1"), { target: { value: "0" } });
    fireEvent.click(screen.getByTestId("invoice-submit"));

    expect(screen.getByTestId("invoice-number-error").textContent).toBe(copy.numberInvalid);
    expect(screen.getByTestId("invoice-error-po-item-1").textContent).toBe(copy.qtyInvalid);
    expect(screen.getByTestId("invoice-form-error").textContent).toBe(copy.minOneLine);
    expect(fetchMock).not.toHaveBeenCalled();

    // A fractional qty on the other line also fails.
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-1"), { target: { value: "2" } });
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-2"), { target: { value: "1.5" } });
    fireEvent.click(screen.getByTestId("invoice-submit"));
    expect(screen.getByTestId("invoice-error-po-item-2").textContent).toBe(copy.qtyInvalid);
    expect(fetchMock).not.toHaveBeenCalled();

    // A negative price fails.
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-2"), { target: { value: "1" } });
    fireEvent.change(screen.getByTestId("invoice-price-po-item-2"), { target: { value: "-5" } });
    fireEvent.click(screen.getByTestId("invoice-submit"));
    expect(screen.getByTestId("invoice-error-po-item-2").textContent).toBe(copy.priceInvalid);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("accepts a zero price (a free line) and computes the live line total", () => {
    renderForm();
    fireEvent.change(screen.getByTestId("invoice-price-po-item-2"), { target: { value: "0" } });
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-2"), { target: { value: "3" } });
    expect(screen.getByTestId("invoice-line-total-po-item-2").textContent?.replace(/\u00A0/g, " ")).toBe("Rp 0");
    expect(screen.queryByTestId("invoice-error-po-item-2")).toBeNull();
  });

  it("POSTs the trimmed number and the filled lines, then redirects to the new detail", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ id: "inv-9", number: "INV/2026/0001" }, 201));
    vi.stubGlobal("fetch", fetchMock);

    renderForm();
    fireEvent.change(screen.getByTestId("invoice-number"), { target: { value: "  INV/2026/0001  " } });
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-1"), { target: { value: "2" } });
    // Line 2 stays empty: not billed by this invoice.
    fireEvent.click(screen.getByTestId("invoice-submit"));

    await waitFor(() => expect(mocks.push).toHaveBeenCalledWith("/app/invoices/inv-9"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/invoices");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      po_id: "po-1",
      number: "INV/2026/0001",
      items: [{ po_item_id: "po-item-1", qty: 2, unit_price_cents: 7500000 }],
    });
  });

  it("renders the pinned 409 inv_over_bill numbers inline", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          {
            error: "inv_over_bill: po_number=PO-000001 invoiced=6000000 requested=6000000 total=10000000",
            code: "inv_over_bill",
          },
          409,
        ),
      ),
    );

    renderForm();
    fireEvent.change(screen.getByTestId("invoice-number"), { target: { value: "INV-1" } });
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-1"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("invoice-submit"));

    const error = await screen.findByTestId("invoice-form-error");
    // The parsed amounts, formatted as money (id-ID IDR: cents / 100).
    expect(error.textContent?.replace(/\u00A0/g, " ")).toBe(
      "Over-bills PO PO-000001: Rp 60.000 already invoiced and this invoice requests Rp 60.000 more, but the total is Rp 100.000.",
    );
    expect(mocks.push).not.toHaveBeenCalled();
  });

  it("maps another machine code (409 inv_number_duplicate) inline", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({ error: "duplicate", code: "inv_number_duplicate" }, 409),
      ),
    );

    renderForm();
    fireEvent.change(screen.getByTestId("invoice-number"), { target: { value: "INV-1" } });
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-1"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("invoice-submit"));

    const error = await screen.findByTestId("invoice-form-error");
    expect(error.textContent).toBe(copy.errorMessages.inv_number_duplicate);
  });

  it("runs the refresh dance on 401 with the session-expired notice", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Session expired." }, 401)));

    renderForm();
    fireEvent.change(screen.getByTestId("invoice-number"), { target: { value: "INV-1" } });
    fireEvent.change(screen.getByTestId("invoice-qty-po-item-1"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("invoice-submit"));

    await waitFor(() =>
      expect(mocks.assign).toHaveBeenCalledWith(
        "/api/auth/refresh?next=%2Fapp%2Finvoices%2Fnew%3Fpo%3Dpo-1",
      ),
    );
    expect((await screen.findByTestId("invoice-form-error")).textContent).toBe(copy.sessionExpired);
  });
});

describe("parseOverBill", () => {
  it("extracts the pinned numbers and rejects other shapes", () => {
    expect(parseOverBill("inv_over_bill: po_number=PO-7 invoiced=1 requested=2 total=3")).toEqual({
      poNumber: "PO-7",
      invoiced: 1,
      requested: 2,
      total: 3,
    });
    expect(parseOverBill("something else")).toBeNull();
    expect(parseOverBill("")).toBeNull();
  });
});
