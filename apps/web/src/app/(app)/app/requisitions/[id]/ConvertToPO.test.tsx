// ConvertToPO tests (issue #112): vendor options render from props, submit
// stays disabled until a vendor is picked, the POST body/path carry the
// chosen vendor_id and requisition id, success distinguishes 201 (created)
// from 200 (#35 idempotent replay — different copy, same PO link, always a
// router.refresh), and 4xx machine codes render as mapped messages with 401
// running the refresh dance.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import ConvertToPO from "./ConvertToPO";

const mocks = vi.hoisted(() => ({
  refresh: vi.fn(),
  assign: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: mocks.refresh }),
  usePathname: () => "/app/requisitions/pr-1",
}));

const copy = {
  action: "Create PO",
  vendorLabel: "Vendor",
  vendorPlaceholder: "Pick an active vendor…",
  submitting: "Creating the PO…",
  noVendors: "No active vendors — activate a vendor on the Vendors page before creating a PO.",
  loadFailed: "The vendor list failed to load — reload the page to retry.",
  createdTitle: "Draft PO created",
  replayedTitle: "This requisition was already converted to a PO",
  openPo: "Open PO",
  genericError: "Failed to create the PO — try again.",
  sessionExpired: "Session expired — redirecting to sign-in…",
  errorMessages: {
    po_requisition_not_approved: "Only an approved requisition can be converted.",
    vendor_deactivated: "That vendor is deactivated — pick another vendor.",
  },
};

const VENDOR_A = "11111111-2222-4333-8444-555555555555";
const VENDOR_B = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee";
const vendors = [
  { id: VENDOR_A, code: "ACME-01", name: "Acme Supplies" },
  { id: VENDOR_B, code: "ACME-02", name: "Acme Logistics" },
];

const PO = { id: "po-1", number: "PO-000001", status: "draft", items: [] };

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

function renderForm(vendorProp: typeof vendors | null = vendors) {
  return render(<ConvertToPO requisitionId="pr-1" vendors={vendorProp} copy={copy} />);
}

function pickVendor() {
  fireEvent.change(screen.getByTestId("convert-vendor"), { target: { value: VENDOR_A } });
}

describe("ConvertToPO", () => {
  it("renders the vendor options from props with submit disabled until one is picked", () => {
    renderForm();
    const select = screen.getByTestId("convert-vendor") as HTMLSelectElement;
    expect(select.options).toHaveLength(3); // placeholder + the two vendors
    expect(select.options.item(1)?.textContent).toBe("ACME-01 — Acme Supplies");
    expect((screen.getByTestId("convert-submit") as HTMLButtonElement).disabled).toBe(true);

    pickVendor();
    expect((screen.getByTestId("convert-submit") as HTMLButtonElement).disabled).toBe(false);
  });

  it("shows the actionable load-failed message when the vendor list is null", () => {
    renderForm(null);
    expect(screen.getByTestId("convert-load-failed").textContent).toBe(copy.loadFailed);
    expect(screen.queryByTestId("convert-vendor")).toBeNull();
  });

  it("shows the actionable empty message when the tenant has no active vendors", () => {
    renderForm([]);
    expect(screen.getByTestId("convert-no-vendors").textContent).toBe(copy.noVendors);
    expect(screen.queryByTestId("convert-vendor")).toBeNull();
  });

  it("POSTs the chosen vendor_id to the from-requisition BFF route", async () => {
    const fetchMock = vi.fn(async () => jsonResponse(PO, 201));
    vi.stubGlobal("fetch", fetchMock);

    renderForm();
    pickVendor();
    fireEvent.click(screen.getByTestId("convert-submit"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/purchase-orders/from-requisition/pr-1");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ vendor_id: VENDOR_A });
  });

  it("201 renders the created confirmation with the PO number, link, and refresh", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(PO, 201)));

    renderForm();
    pickVendor();
    fireEvent.click(screen.getByTestId("convert-submit"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    expect(screen.getByTestId("convert-success-title").textContent).toBe(copy.createdTitle);
    expect(screen.getByTestId("convert-po-number").textContent).toBe("PO-000001");
    const link = screen.getByTestId("convert-po-link") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/app/purchase-orders/po-1");
    expect(screen.queryByTestId("convert-error")).toBeNull();
  });

  it("200 renders the replay confirmation (already converted is a success, different copy)", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(PO, 200)));

    renderForm();
    pickVendor();
    fireEvent.click(screen.getByTestId("convert-submit"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    expect(screen.getByTestId("convert-success-title").textContent).toBe(copy.replayedTitle);
    expect(screen.getByTestId("convert-po-number").textContent).toBe("PO-000001");
    expect((screen.getByTestId("convert-po-link") as HTMLAnchorElement).getAttribute("href")).toBe(
      "/app/purchase-orders/po-1",
    );
  });

  it("shows the mapped message for a 4xx machine code (lost race)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          jsonResponse(
            { error: "only an approved requisition can be converted", code: "po_requisition_not_approved" },
            409,
          ),
      ),
    );

    renderForm();
    pickVendor();
    fireEvent.click(screen.getByTestId("convert-submit"));

    const error = await screen.findByTestId("convert-error");
    expect(error.textContent).toBe(copy.errorMessages.po_requisition_not_approved);
    expect(mocks.refresh).not.toHaveBeenCalled();
    expect(screen.queryByTestId("convert-success")).toBeNull();
  });

  it("falls back to the generic message for an unmapped code", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "boom", code: "something_new" }, 412)));

    renderForm();
    pickVendor();
    fireEvent.click(screen.getByTestId("convert-submit"));

    const error = await screen.findByTestId("convert-error");
    expect(error.textContent).toBe(copy.genericError);
  });

  it("runs the refresh dance on 401 with the session-expired notice", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Session expired." }, 401)));

    renderForm();
    pickVendor();
    fireEvent.click(screen.getByTestId("convert-submit"));

    await waitFor(() =>
      expect(mocks.assign).toHaveBeenCalledWith("/api/auth/refresh?next=%2Fapp%2Frequisitions%2Fpr-1"),
    );
    expect((await screen.findByTestId("convert-error")).textContent).toBe(copy.sessionExpired);
  });
});
