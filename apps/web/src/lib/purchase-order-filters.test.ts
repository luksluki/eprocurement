// purchase-order-filters tests (issue #38): parse/serialize round trips,
// clamping that mirrors the server's ValidateList, and unknown-status drops
// — the requisition-filters (#32) conventions applied to the seven PO
// statuses.
import { describe, expect, it } from "vitest";
import {
  DEFAULT_PAGE_SIZE,
  MAX_PAGE_SIZE,
  PAGE_SIZE_OPTIONS,
  PURCHASE_ORDER_STATUSES,
  filtersToSearchParams,
  parseFilters,
  withPage,
} from "./purchase-order-filters";

function params(entries: Record<string, string>): URLSearchParams {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(entries)) p.set(k, v);
  return p;
}

describe("parseFilters", () => {
  it("applies the defaults for an empty query", () => {
    expect(parseFilters(new URLSearchParams())).toEqual({
      q: "",
      status: "",
      page: 1,
      pageSize: DEFAULT_PAGE_SIZE,
    });
  });

  it("keeps a known status and trims the query", () => {
    for (const status of PURCHASE_ORDER_STATUSES) {
      expect(parseFilters(params({ status, q: "  PO-0001  " })).status).toBe(status);
    }
    expect(parseFilters(params({ q: "  PO-0001  " })).q).toBe("PO-0001");
  });

  it("drops unknown statuses (a requisition status is not a PO status)", () => {
    expect(parseFilters(params({ status: "submitted" })).status).toBe("");
    expect(parseFilters(params({ status: "paused" })).status).toBe("");
  });

  it("clamps page/page_size the way the server does", () => {
    expect(parseFilters(params({ page: "0", page_size: "0" }))).toMatchObject({ page: 1, pageSize: DEFAULT_PAGE_SIZE });
    expect(parseFilters(params({ page: "abc", page_size: "xyz" }))).toMatchObject({
      page: 1,
      pageSize: DEFAULT_PAGE_SIZE,
    });
    expect(parseFilters(params({ page_size: "500" })).pageSize).toBe(MAX_PAGE_SIZE);
    expect(parseFilters(params({ page_size: "2" })).pageSize).toBe(2);
  });

  it("caps absurdly long queries", () => {
    expect(parseFilters(params({ q: "x".repeat(150) })).q.length).toBe(100);
  });
});

describe("filtersToSearchParams", () => {
  it("omits defaults so one view maps to one URL", () => {
    expect(filtersToSearchParams({ q: "", status: "", page: 1, pageSize: DEFAULT_PAGE_SIZE }).toString()).toBe("");
    expect(
      filtersToSearchParams({ q: "PO-0001", status: "issued", page: 3, pageSize: 50 }).toString(),
    ).toBe("q=PO-0001&status=issued&page=3&page_size=50");
  });

  it("round-trips through parseFilters", () => {
    const filters = { q: "PO-38", status: "draft" as const, page: 4, pageSize: 100 };
    const parsed = parseFilters(filtersToSearchParams(filters));
    expect(parsed).toEqual(filters);
  });
});

describe("withPage", () => {
  it("changes only the page, keeping the active filters", () => {
    expect(withPage({ q: "PO-1", status: "issued", page: 1, pageSize: 50 }, 7)).toEqual({
      q: "PO-1",
      status: "issued",
      page: 7,
      pageSize: 50,
    });
  });
});

describe("PAGE_SIZE_OPTIONS", () => {
  it("stays within the server's max", () => {
    for (const n of PAGE_SIZE_OPTIONS) expect(n).toBeLessThanOrEqual(MAX_PAGE_SIZE);
  });
});
