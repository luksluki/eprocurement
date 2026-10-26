// RequisitionWizard wiring tests (#33): step guards (RHF+Zod mirror the
// domain rules), autosave creating the draft and locking the currency,
// restore from ?draft=<id> (happy + non-draft read-only + not-found), and
// the guarded final submit. next/navigation and fetch are mocked.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import RequisitionWizard, { type WizardCopy } from "./RequisitionWizard";
import type { RequisitionDetail } from "@/lib/gateway";

const nav = vi.hoisted(() => ({
  push: vi.fn(),
  replace: vi.fn(),
  pathname: "/app/requisitions/new",
  searchParams: new URLSearchParams(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: nav.push, replace: nav.replace }),
  usePathname: () => nav.pathname,
  useSearchParams: () => nav.searchParams,
}));

const copy: WizardCopy = {
  steps: { details: "Details", items: "Items", attachments: "Attachments", review: "Review" },
  details: {
    heading: "Requisition details",
    currencyLabel: "Currency",
    currencyPlaceholder: "Choose a currency…",
    currencyLockedNote: "Currency is locked once the draft is created.",
    next: "Continue to items",
  },
  items: {
    heading: "Requisition items",
    description: "Description",
    qty: "Qty",
    unitPrice: "Unit price (cents)",
    category: "Category",
    add: "Add item",
    remove: "Remove item",
    next: "Continue to attachments",
    back: "Back",
  },
  attachments: {
    heading: "Attachments",
    hint: "Files upload directly via a presigned URL.",
    add: "Choose a file",
    empty: "No attachments yet.",
    uploading: "Uploading…",
    next: "Continue to review",
    back: "Back",
  },
  review: {
    heading: "Review and submit",
    itemsHeading: "Items",
    attachmentsHeading: "Attachments",
    total: "Total (cents)",
    submit: "Submit for approval",
    submitting: "Submitting…",
    back: "Back",
    itemsEmpty: "Add at least one item before submitting.",
  },
  autosave: { saving: "Saving draft…", saved: "Draft saved.", failed: "Failed to save the draft." },
  restore: {
    loading: "Restoring draft…",
    notFound: "Draft not found (or it belongs to another tenant).",
    error: "Failed to load the draft.",
    notDraft: "no longer a draft",
  },
  submitted: { heading: "Requisition submitted:", backToList: "Back to the requisition list" },
};

function draftDetail(overrides: Partial<RequisitionDetail> = {}): RequisitionDetail {
  return {
    id: "pr-9",
    tenant_id: "t-1",
    number: "PR-000009",
    requester_id: "u-1",
    status: "draft",
    currency: "IDR",
    total_cents: 15_000_000,
    items: [{ description: "Laptop", qty: 2, unit_price_cents: 7_500_000, category: "it" }],
    attachments: [
      {
        id: "att-1",
        entity_type: "requisition",
        entity_id: "pr-9",
        filename: "quote.pdf",
        mime_type: "application/pdf",
        size_bytes: 2048,
        checksum_sha256: "ab".repeat(32),
        status: "ready",
        created_by: "u-1",
        created_at: "2026-10-25T10:00:00Z",
        uploaded_at: "2026-10-25T10:01:00Z",
      },
    ],
    approval_steps: [],
    submitted_at: "",
    created_at: "2026-10-25T08:00:00Z",
    created_by: "u-1",
    updated_at: "2026-10-25T08:00:00Z",
    updated_by: "u-1",
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  nav.searchParams = new URLSearchParams();
  nav.replace.mockClear();
  nav.push.mockClear();
  fetchMock = vi.fn(async () => jsonResponse(draftDetail()));
  vi.stubGlobal("fetch", fetchMock);
  if (!globalThis.crypto?.randomUUID) {
    vi.stubGlobal("crypto", { subtle: globalThis.crypto?.subtle, randomUUID: () => "key-test" });
  }
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderWizard() {
  return render(<RequisitionWizard copy={copy} />);
}

describe("RequisitionWizard step guards (#33)", () => {
  it("blocks details→items until a currency is chosen (no default, domain contract)", () => {
    renderWizard();
    expect((screen.getByTestId("next-from-details") as HTMLButtonElement).disabled).toBe(true);
    // Empty choice is rejected by the schema…
    fireEvent.click(screen.getByTestId("next-from-details"));
    expect(screen.queryByTestId("step-panel-items")).toBeNull();
  });

  it("creates the draft via autosave once the currency is valid and locks the field", async () => {
    renderWizard();
    fireEvent.change(screen.getByTestId("input-currency"), { target: { value: "IDR" } });

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/requisitions");
    expect(init.method).toBe("POST");
    expect(String(init.body)).toBe(JSON.stringify({ currency: "IDR" }));

    // The URL now carries ?draft=<id>; the currency select is locked.
    await waitFor(() => expect(nav.replace).toHaveBeenCalledWith("/app/requisitions/new?draft=pr-9", expect.anything()));
    await waitFor(() => expect((screen.getByTestId("input-currency") as HTMLSelectElement).disabled).toBe(true));
    expect((screen.getByTestId("next-from-details") as HTMLButtonElement).disabled).toBe(false);
  });

  it("blocks items→attachments on an invalid row and shows the field error", async () => {
    renderWizard();
    fireEvent.change(screen.getByTestId("input-currency"), { target: { value: "IDR" } });
    await waitFor(() => expect(nav.replace).toHaveBeenCalled());
    fireEvent.click(screen.getByTestId("next-from-details"));
    await waitFor(() => expect(screen.getByTestId("step-panel-items")).toBeTruthy());

    // Blank description and zero qty violate the domain rules.
    fireEvent.change(screen.getByTestId("item-description-0"), { target: { value: "   " } });
    fireEvent.change(screen.getByTestId("item-qty-0"), { target: { value: "0" } });
    fireEvent.click(screen.getByTestId("next-from-items"));

    await waitFor(() =>
      expect(screen.getByTestId("item-error-description-0").textContent).toContain("not be blank"),
    );
    expect(screen.getByTestId("item-error-qty-0").textContent).toContain("greater than zero");
    expect(screen.queryByTestId("step-panel-attachments")).toBeNull();

    // Fix the row: the guard lets the step advance.
    fireEvent.change(screen.getByTestId("item-description-0"), { target: { value: "Laptop" } });
    fireEvent.change(screen.getByTestId("item-qty-0"), { target: { value: "2" } });
    fireEvent.click(screen.getByTestId("next-from-items"));
    await waitFor(() => expect(screen.getByTestId("step-panel-attachments")).toBeTruthy());
    expect(screen.queryByTestId("item-error-description-0")).toBeNull();
  });
});

describe("RequisitionWizard submit guard (#33)", () => {
  it("refuses to submit an empty basket (min one item) with a visible message", async () => {
    renderWizard();
    fireEvent.change(screen.getByTestId("input-currency"), { target: { value: "IDR" } });
    await waitFor(() => expect(nav.replace).toHaveBeenCalled());
    fireEvent.click(screen.getByTestId("next-from-details"));
    await waitFor(() => expect(screen.getByTestId("step-panel-items")).toBeTruthy());

    // Remove the (only) default row → zero items.
    fireEvent.click(screen.getByTestId("item-remove-0"));
    fireEvent.click(screen.getByTestId("next-from-items"));
    await waitFor(() => expect(screen.getByTestId("step-panel-attachments")).toBeTruthy());
    fireEvent.click(screen.getByTestId("next-from-attachments"));
    await waitFor(() => expect(screen.getByTestId("step-panel-review")).toBeTruthy());

    fireEvent.click(screen.getByTestId("submit-requisition"));
    await waitFor(() =>
      expect(screen.getByTestId("submit-error").textContent).toContain("Add at least one item"),
    );
    const calls = fetchMock.mock.calls as unknown as Array<[string, RequestInit]>;
    expect(calls.some(([url, init]) => url === "/api/requisitions/pr-9/submit" && init.method === "POST")).toBe(false);
  });

  it("submits a valid draft and shows the assigned PR number", async () => {
    renderWizard();
    fireEvent.change(screen.getByTestId("input-currency"), { target: { value: "IDR" } });
    await waitFor(() => expect(nav.replace).toHaveBeenCalled());
    fireEvent.click(screen.getByTestId("next-from-details"));
    await waitFor(() => expect(screen.getByTestId("step-panel-items")).toBeTruthy());
    fireEvent.change(screen.getByTestId("item-description-0"), { target: { value: "Laptop" } });
    fireEvent.click(screen.getByTestId("next-from-items"));
    await waitFor(() => expect(screen.getByTestId("step-panel-attachments")).toBeTruthy());
    fireEvent.click(screen.getByTestId("next-from-attachments"));
    await waitFor(() => expect(screen.getByTestId("step-panel-review")).toBeTruthy());

    fetchMock.mockImplementation(
      async () => jsonResponse(draftDetail({ status: "submitted", submitted_at: "2026-10-25T10:30:00Z" })),
    );
    fireEvent.click(screen.getByTestId("submit-requisition"));
    await waitFor(() => expect(screen.getByTestId("submitted").textContent).toContain("PR-000009"));
    const calls = fetchMock.mock.calls as unknown as Array<[string, RequestInit]>;
    const submitCall = calls.find(([url]) => url === "/api/requisitions/pr-9/submit");
    expect(submitCall).toBeDefined();
    expect(submitCall?.[1].method).toBe("POST");
  });
});

describe("RequisitionWizard restore (#33)", () => {
  it("restores header, items, and attachments from ?draft=<id>", async () => {
    nav.searchParams = new URLSearchParams("?draft=pr-9");
    renderWizard();

    // Header restored (currency locked at the draft's value)…
    await waitFor(() =>
      expect((screen.getByTestId("input-currency") as HTMLSelectElement).disabled).toBe(true),
    );
    expect((screen.getByTestId("input-currency") as HTMLSelectElement).value).toBe("IDR");
    // …items restored into the basket…
    fireEvent.click(screen.getByTestId("next-from-details"));
    await waitFor(() => expect(screen.getByTestId("step-panel-items")).toBeTruthy());
    await waitFor(() =>
      expect((screen.getByTestId("item-description-0") as HTMLInputElement).value).toBe("Laptop"),
    );
    expect((screen.getByTestId("item-qty-0") as HTMLInputElement).value).toBe("2");
    // …and the attachment list restored (embedded in the same GET).
    fireEvent.click(screen.getByTestId("next-from-items"));
    await waitFor(() => expect(screen.getByTestId("step-panel-attachments")).toBeTruthy());
    await waitFor(() => expect(screen.getByTestId("attachment-att-1").textContent).toContain("quote.pdf"));

    // The restored basket must not immediately re-PUT: after the restore
    // GET, no further calls happen.
    const callsAfterRestore = (fetchMock.mock.calls as unknown as Array<[string, RequestInit]>).length;
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(fetchMock.mock.calls).toHaveLength(callsAfterRestore);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/requisitions/pr-9");
  });

  it("flips read-only (and stops autosave) when the restored requisition is not a draft", async () => {
    nav.searchParams = new URLSearchParams("?draft=pr-9");
    fetchMock.mockImplementation(async () => jsonResponse(draftDetail({ status: "submitted" })));
    renderWizard();

    await waitFor(() => expect(screen.getByTestId("readonly-notice").textContent).toContain("no longer a draft"));
    // Read-only: the wizard refuses to advance (and thus to submit) — the
    // restored data is visible on the details step only.
    await waitFor(() =>
      expect((screen.getByTestId("next-from-details") as HTMLButtonElement).disabled).toBe(true),
    );
    expect(screen.queryByTestId("step-panel-items")).toBeNull();
  });

  it("shows an error state without a form when the draft does not exist (cross-tenant 404)", async () => {
    nav.searchParams = new URLSearchParams("?draft=pr-404");
    fetchMock.mockImplementation(async () =>
      jsonResponse({ error: "requisition not found", code: "requisition_not_found" }, 404),
    );
    renderWizard();

    await waitFor(() => expect(screen.getByTestId("restore-error").textContent).toContain("not found"));
    expect(screen.queryByTestId("step-panel-details")).toBeNull();
  });
});
