// InvoiceActions tests (issue #41): render/hide follows the eligibility
// prop, the hard attachment gate DISABLES the Match button until a ready
// attachment exists (maintainer decision 2026-10-29 — the server never
// checks, #40), a success POSTs and refreshes the server component, a 409
// inv_already_matched maps inline, and a 401 runs the refresh dance.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import InvoiceActions from "./InvoiceActions";

const mocks = vi.hoisted(() => ({
  refresh: vi.fn(),
  assign: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: mocks.refresh }),
  usePathname: () => "/app/invoices/inv-1",
}));

const copy = {
  match: "Run match",
  submitting: "Matching…",
  gated: "Upload and complete a vendor invoice attachment (ready status) before matching.",
  genericError: "Failed to run the match — try again.",
  sessionExpired: "Session expired — redirecting to sign-in…",
  errorMessages: {
    inv_already_matched: "This invoice is already matched; the result is final.",
    inv_not_found: "Invoice not found.",
  },
};

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

describe("InvoiceActions (the Match button)", () => {
  it("renders nothing when not eligible (viewer or matched invoice)", () => {
    const { container } = render(
      <InvoiceActions invoiceId="inv-1" canMatch={false} attachmentReady={false} copy={copy} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("is DISABLED with the gate reason while no attachment is ready (hard gate)", () => {
    render(<InvoiceActions invoiceId="inv-1" canMatch={true} attachmentReady={false} copy={copy} />);
    const button = screen.getByTestId("invoice-match") as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(screen.getByTestId("invoice-match-gate").textContent).toBe(copy.gated);
  });

  it("is ENABLED once an attachment is ready, POSTs the match, and refreshes", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ id: "inv-1", status: "matched" }));
    vi.stubGlobal("fetch", fetchMock);

    render(<InvoiceActions invoiceId="inv-1" canMatch={true} attachmentReady={true} copy={copy} />);
    const button = screen.getByTestId("invoice-match") as HTMLButtonElement;
    expect(button.disabled).toBe(false);
    expect(screen.queryByTestId("invoice-match-gate")).toBeNull();

    fireEvent.click(button);
    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/invoices/inv-1/match");
    expect(init.method).toBe("POST");
    expect(screen.queryByTestId("invoice-match-error")).toBeNull();
  });

  it("maps a 409 inv_already_matched inline (a concurrent match won the race)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({ error: "this invoice is already matched", code: "inv_already_matched" }, 409),
      ),
    );

    render(<InvoiceActions invoiceId="inv-1" canMatch={true} attachmentReady={true} copy={copy} />);
    fireEvent.click(screen.getByTestId("invoice-match"));

    const error = await screen.findByTestId("invoice-match-error");
    expect(error.textContent).toBe(copy.errorMessages.inv_already_matched);
    expect(mocks.refresh).not.toHaveBeenCalled();
  });

  it("falls back to the generic message for an unmapped code", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "boom", code: "something_new" }, 400)));

    render(<InvoiceActions invoiceId="inv-1" canMatch={true} attachmentReady={true} copy={copy} />);
    fireEvent.click(screen.getByTestId("invoice-match"));

    const error = await screen.findByTestId("invoice-match-error");
    expect(error.textContent).toBe(copy.genericError);
  });

  it("runs the refresh dance on 401 with the session-expired notice", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Session expired." }, 401)));

    render(<InvoiceActions invoiceId="inv-1" canMatch={true} attachmentReady={true} copy={copy} />);
    fireEvent.click(screen.getByTestId("invoice-match"));

    await waitFor(() => expect(mocks.assign).toHaveBeenCalledWith("/api/auth/refresh?next=%2Fapp%2Finvoices%2Finv-1"));
    expect((await screen.findByTestId("invoice-match-error")).textContent).toBe(copy.sessionExpired);
  });
});
