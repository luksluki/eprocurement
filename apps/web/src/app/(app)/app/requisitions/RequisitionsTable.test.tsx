// RequisitionsTable wiring tests (issue #32): the acceptance criterion
// "filters persist in the URL" is glue that requisition-filters.test.ts
// cannot see — useSearchParams → parseFilters → rendered controls, and
// filter changes → router.push with a canonical query and the page reset
// to 1. next/navigation and the BFF fetch are mocked; a QueryClientProvider
// wrapper supplies the TanStack Query context.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import RequisitionsTable from "./RequisitionsTable";
import type { RequisitionHeader, RequisitionListPage } from "@/lib/gateway";

// next/navigation only works inside the App Router; mock the three hooks the
// component consumes. vi.hoisted keeps the mutable handles reachable from the
// hoisted vi.mock factory.
const nav = vi.hoisted(() => ({
  push: vi.fn(),
  pathname: "/app/requisitions",
  searchParams: new URLSearchParams(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: nav.push }),
  usePathname: () => nav.pathname,
  useSearchParams: () => nav.searchParams,
}));

function row(number: string): RequisitionHeader {
  const now = "2026-10-01T08:00:00Z";
  return {
    id: `id-${number}`,
    tenant_id: "tenant-1",
    number,
    requester_id: "user-1",
    status: "draft",
    currency: "IDR",
    total_cents: 150_000,
    items: [],
    approval_steps: [],
    submitted_at: now,
    created_at: now,
    updated_at: now,
    created_by: "user-1",
    updated_by: "user-1",
  };
}

function makePage(
  numbers: string[],
  meta: { page: number; page_size: number; total?: number },
): RequisitionListPage {
  return {
    requisitions: numbers.map(row),
    total: meta.total ?? numbers.length,
    page: meta.page,
    page_size: meta.page_size,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

function stubBff(responder: Response | ((url: string) => Response)) {
  const fetchMock = vi.fn(async (url: string) => (typeof responder === "function" ? responder(url) : responder));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderTable(query = "") {
  nav.searchParams = new URLSearchParams(query);
  nav.push.mockClear();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <RequisitionsTable />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("RequisitionsTable URL filter wiring", () => {
  it("parses filters from the URL into the controls and forwards them to the BFF", async () => {
    const fetchMock = stubBff(jsonResponse(makePage(["PR-000101", "PR-000102"], { page: 2, page_size: 50, total: 120 })));

    renderTable("q=PR-0001&status=draft&page=2&page_size=50");

    expect((screen.getByTestId("filter-q") as HTMLInputElement).value).toBe("PR-0001");
    expect((screen.getByTestId("filter-status") as HTMLSelectElement).value).toBe("draft");
    expect((screen.getByTestId("filter-page-size") as HTMLSelectElement).value).toBe("50");

    // Rows render, then the canonical query string reached the BFF.
    expect(await screen.findByText("PR-000101")).toBeTruthy();
    expect(screen.getByText("PR-000102")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledWith("/api/requisitions?q=PR-0001&status=draft&page=2&page_size=50");
  });

  it("pushes a canonical URL with the page reset to 1 on every filter change", () => {
    stubBff(jsonResponse(makePage([], { page: 3, page_size: 50, total: 0 })));

    renderTable("q=old&status=draft&page=3&page_size=50");

    // Status change: page 3 is reset to 1, so `page` disappears from the URL.
    fireEvent.change(screen.getByTestId("filter-status"), { target: { value: "approved" } });
    expect(nav.push).toHaveBeenCalledWith("/app/requisitions?q=old&status=approved&page_size=50", { scroll: false });

    // Search submit: same reset, new q.
    fireEvent.change(screen.getByTestId("filter-q"), { target: { value: "PR-0009" } });
    fireEvent.click(screen.getByTestId("filter-apply"));
    expect(nav.push).toHaveBeenCalledWith("/app/requisitions?q=PR-0009&status=draft&page_size=50", { scroll: false });

    // Page-size change resets the page too.
    fireEvent.change(screen.getByTestId("filter-page-size"), { target: { value: "100" } });
    expect(nav.push).toHaveBeenCalledWith("/app/requisitions?q=old&status=draft&page_size=100", { scroll: false });
  });

  it("keeps the filters in the URL when paginating", async () => {
    stubBff(jsonResponse(makePage(["PR-000001"], { page: 2, page_size: 20, total: 60 })));

    renderTable("q=pr&status=submitted&page=2");

    fireEvent.click(await screen.findByTestId("next-page"));
    expect(nav.push).toHaveBeenCalledWith("/app/requisitions?q=pr&status=submitted&page=3", { scroll: false });

    fireEvent.click(screen.getByTestId("prev-page"));
    // Going back to page 1 yields the canonical URL without `page`.
    expect(nav.push).toHaveBeenLastCalledWith("/app/requisitions?q=pr&status=submitted", { scroll: false });
  });

  it("renders the BFF error state with a retry when the list request fails", async () => {
    const fetchMock = stubBff(jsonResponse({ error: "boom" }, 500));

    renderTable("status=draft");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Failed to load requisitions");
    expect(screen.getByTestId("retry")).toBeTruthy();

    fireEvent.click(screen.getByTestId("retry"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
  });
});
