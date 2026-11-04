// POActions tests (issue #38): render/hide follows the eligibility prop
// (null hides — AC "actions hidden when not eligible"), a success POSTs and
// refreshes the server component, a 409 maps to the machine-code message,
// and a 401 runs the refresh dance.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import POActions from "./POActions";

const mocks = vi.hoisted(() => ({
  refresh: vi.fn(),
  assign: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: mocks.refresh }),
  usePathname: () => "/app/purchase-orders/po-1",
}));

const copy = {
  issue: "Issue",
  acknowledge: "Acknowledge vendor",
  submitting: "Working…",
  genericError: "Failed — try again.",
  sessionExpired: "Session expired — redirecting to sign-in…",
  errorMessages: {
    po_illegal_transition: "The purchase order cannot move that way from its current status.",
    po_not_found: "Purchase order not found.",
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

describe("POActions", () => {
  it("renders the issue button when the kind is issue", () => {
    render(<POActions poId="po-1" kind="issue" copy={copy} />);
    expect(screen.getByTestId("po-issue").textContent).toBe("Issue");
    expect(screen.queryByTestId("po-acknowledge")).toBeNull();
  });

  it("renders the acknowledge button when the kind is acknowledge", () => {
    render(<POActions poId="po-1" kind="acknowledge" copy={copy} />);
    expect(screen.getByTestId("po-acknowledge").textContent).toBe("Acknowledge vendor");
  });

  it("renders nothing when the kind is null (not eligible)", () => {
    const { container } = render(<POActions poId="po-1" kind={null} copy={copy} />);
    expect(container.firstChild).toBeNull();
    expect(screen.queryByTestId("po-issue")).toBeNull();
  });

  it("POSTs to the action route and refreshes on success", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ id: "po-1", status: "issued" }));
    vi.stubGlobal("fetch", fetchMock);

    render(<POActions poId="po-1" kind="issue" copy={copy} />);
    fireEvent.click(screen.getByTestId("po-issue"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/purchase-orders/po-1/issue");
    expect(init.method).toBe("POST");
    expect(screen.queryByTestId("po-action-error")).toBeNull();
  });

  it("shows the mapped message for a 409 machine code (lost race)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          { error: "the purchase order cannot move that way", code: "po_illegal_transition" },
          409,
        ),
      ),
    );

    render(<POActions poId="po-1" kind="acknowledge" copy={copy} />);
    fireEvent.click(screen.getByTestId("po-acknowledge"));

    const error = await screen.findByTestId("po-action-error");
    expect(error.textContent).toBe(copy.errorMessages.po_illegal_transition);
    expect(mocks.refresh).not.toHaveBeenCalled();
  });

  it("falls back to the generic message for an unmapped code", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "boom", code: "something_new" }, 400)));

    render(<POActions poId="po-1" kind="issue" copy={copy} />);
    fireEvent.click(screen.getByTestId("po-issue"));

    const error = await screen.findByTestId("po-action-error");
    expect(error.textContent).toBe(copy.genericError);
  });

  it("runs the refresh dance on 401 with the session-expired notice", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Session expired." }, 401)));

    render(<POActions poId="po-1" kind="issue" copy={copy} />);
    fireEvent.click(screen.getByTestId("po-issue"));

    await waitFor(() =>
      expect(mocks.assign).toHaveBeenCalledWith("/api/auth/refresh?next=%2Fapp%2Fpurchase-orders%2Fpo-1"),
    );
    expect((await screen.findByTestId("po-action-error")).textContent).toBe(copy.sessionExpired);
  });
});
