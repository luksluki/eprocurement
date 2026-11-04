import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { pageCountOf, TablePagination, type PaginationSummaryRange } from "./TablePagination";

afterEach(() => {
  cleanup();
});

function labels() {
  return {
    nav: "Pagination",
    previous: "Previous",
    next: "Next",
    summary: (range: PaginationSummaryRange) =>
      `Showing ${range.from}–${range.to} of ${range.total} (page ${range.page} of ${range.pageCount})`,
  };
}

describe("TablePagination (server mode)", () => {
  it("computes the range and page count for a 100000-row stub", () => {
    const summary = vi.fn(labels().summary);
    const { container } = render(
      <TablePagination
        page={1}
        pageSize={50}
        total={100_000}
        hrefFor={(page) => `/rows?page=${page}`}
        labels={{ ...labels(), summary }}
      />,
    );

    expect(summary).toHaveBeenCalledWith({ from: 1, to: 50, total: 100_000, page: 1, pageCount: 2000 });
    expect(container.textContent).toContain("Showing 1–50 of 100000 (page 1 of 2000)");
  });

  it("renders prev disabled and next as a link on the first page", () => {
    const { container } = render(
      <TablePagination page={1} pageSize={50} total={100_000} hrefFor={(page) => `/rows?page=${page}`} labels={labels()} />,
    );

    const previous = container.querySelectorAll("span[aria-disabled=true]");
    expect(previous).toHaveLength(1);
    expect(previous[0].textContent).toBe("Previous");
    const next = container.querySelector('a[href="/rows?page=2"]');
    expect(next?.textContent).toBe("Next");
    expect(next?.getAttribute("rel")).toBe("next");
  });

  it("renders next disabled and prev as a link on the last page", () => {
    const { container } = render(
      <TablePagination page={2000} pageSize={50} total={100_000} hrefFor={(page) => `/rows?page=${page}`} labels={labels()} />,
    );

    const next = container.querySelectorAll("span[aria-disabled=true]");
    expect(next).toHaveLength(1);
    expect(next[0].textContent).toBe("Next");
    const previous = container.querySelector('a[href="/rows?page=1999"]');
    expect(previous?.textContent).toBe("Previous");
    expect(previous?.getAttribute("rel")).toBe("prev");
  });

  it("uses the injected link component when provided", () => {
    const { container } = render(
      <TablePagination
        page={2}
        pageSize={50}
        total={100_000}
        hrefFor={(page) => `/rows?page=${page}`}
        link={({ href, children, ...rest }) => (
          <a data-testid="page-link" href={href} {...rest}>
            {children}
          </a>
        )}
        labels={labels()}
      />,
    );

    const links = container.querySelectorAll('a[data-testid="page-link"]');
    expect(links).toHaveLength(2);
    expect(links[0].getAttribute("href")).toBe("/rows?page=1");
    expect(links[1].getAttribute("href")).toBe("/rows?page=3");
    expect(links[0].className).toContain("focus-ring");
  });

  it("keeps both controls disabled for an empty result set (failure path)", () => {
    const { container } = render(
      <TablePagination page={1} pageSize={50} total={0} hrefFor={(page) => `/rows?page=${page}`} labels={labels()} />,
    );
    expect(container.querySelectorAll("span[aria-disabled=true]")).toHaveLength(2);
    expect(container.textContent).toContain("Showing 0–0 of 0 (page 1 of 1)");
  });
});

describe("pageCountOf", () => {
  it.each([
    { total: 100_000, pageSize: 50, expected: 2000 },
    { total: 101, pageSize: 50, expected: 3 },
    { total: 50, pageSize: 50, expected: 1 },
    { total: 0, pageSize: 50, expected: 1 },
  ])("returns $expected pages for total=$total pageSize=$pageSize", ({ total, pageSize, expected }) => {
    expect(pageCountOf(total, pageSize)).toBe(expected);
  });
});
