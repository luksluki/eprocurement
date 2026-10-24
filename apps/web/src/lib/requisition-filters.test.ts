import { describe, expect, it } from "vitest";
import {
  DEFAULT_PAGE_SIZE,
  MAX_PAGE_SIZE,
  REQUISITION_STATUSES,
  filtersToSearchParams,
  hasActiveFilters,
  parseFilters,
  withFilters,
  type RequisitionFilters,
} from "./requisition-filters";

function fromQuery(query: string): RequisitionFilters {
  return parseFilters(new URLSearchParams(query));
}

describe("parseFilters", () => {
  it("applies defaults for an empty query string", () => {
    expect(fromQuery("")).toEqual({ q: "", status: "", page: 1, pageSize: DEFAULT_PAGE_SIZE });
  });

  it("reads all four filters", () => {
    expect(fromQuery("q=pr-0001&status=draft&page=3&page_size=50")).toEqual({
      q: "pr-0001",
      status: "draft",
      page: 3,
      pageSize: 50,
    });
  });

  it("trims q and caps its length", () => {
    expect(fromQuery(`q=${" ".repeat(3)}x${" ".repeat(3)}`).q).toBe("x");
    expect(fromQuery(`q=${"x".repeat(150)}`).q.length).toBe(100);
  });

  it("drops unknown statuses instead of forwarding garbage", () => {
    expect(fromQuery("status=paused").status).toBe("");
    expect(fromQuery("status=DRAFT").status).toBe(""); // exact match only
  });

  it("accepts every lifecycle status", () => {
    for (const status of REQUISITION_STATUSES) {
      expect(fromQuery(`status=${status}`).status).toBe(status);
    }
  });

  it("clamps page and page_size like the server does", () => {
    expect(fromQuery("page=0").page).toBe(1);
    expect(fromQuery("page=-5").page).toBe(1);
    expect(fromQuery("page=not-a-number").page).toBe(1);
    expect(fromQuery("page=99999999").page).toBeLessThanOrEqual(1_000_000);
    expect(fromQuery("page_size=500").pageSize).toBe(MAX_PAGE_SIZE);
    expect(fromQuery("page_size=0").pageSize).toBe(DEFAULT_PAGE_SIZE);
    expect(fromQuery("page_size=abc").pageSize).toBe(DEFAULT_PAGE_SIZE);
  });
});

describe("filtersToSearchParams", () => {
  it("omits defaults so one view maps to one URL", () => {
    expect(filtersToSearchParams({ q: "", status: "", page: 1, pageSize: DEFAULT_PAGE_SIZE }).toString()).toBe("");
  });

  it("serializes non-default values", () => {
    expect(filtersToSearchParams({ q: "PR-0001", status: "draft", page: 3, pageSize: 50 }).toString()).toBe(
      "q=PR-0001&status=draft&page=3&page_size=50",
    );
  });

  it("round-trips through parseFilters", () => {
    const filters: RequisitionFilters = { q: "pr-2", status: "submitted", page: 4, pageSize: 100 };
    expect(parseFilters(filtersToSearchParams(filters))).toEqual({ ...filters, q: "pr-2" });
  });
});

describe("withFilters / hasActiveFilters", () => {
  it("resets the page to 1 on any filter change", () => {
    const current: RequisitionFilters = { q: "old", status: "draft", page: 7, pageSize: 50 };
    expect(withFilters(current, { q: "new" }).page).toBe(1);
    expect(withFilters(current, { status: "approved" }).page).toBe(1);
    expect(withFilters(current, { pageSize: 100 }).page).toBe(1);
  });

  it("keeps untouched fields", () => {
    const current: RequisitionFilters = { q: "x", status: "draft", page: 7, pageSize: 50 };
    expect(withFilters(current, { q: "y" })).toEqual({ q: "y", status: "draft", page: 1, pageSize: 50 });
  });

  it("reports whether a user filter is active", () => {
    expect(hasActiveFilters({ q: "", status: "", page: 1, pageSize: 20 })).toBe(false);
    expect(hasActiveFilters({ q: "pr", status: "", page: 1, pageSize: 20 })).toBe(true);
    expect(hasActiveFilters({ q: "", status: "draft", page: 1, pageSize: 20 })).toBe(true);
  });
});
