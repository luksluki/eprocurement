// URL filter model for the invoice list (issue #41), mirroring the
// purchase-order list's (lib/purchase-order-filters, #38) which itself
// mirrors the requisition list (#32). The URL is the single source of truth;
// the list page renders as a server component with a plain GET form, so
// filtering and paging work without client JavaScript. Values are
// canonicalized so an equivalent view always maps to one URL — mirroring the
// server-side clamping in services/procurement/invoice.ValidateList.

export const INVOICE_STATUSES = ["pending", "matched", "discrepancy"] as const;

export type InvoiceStatus = (typeof INVOICE_STATUSES)[number];
export type InvoiceStatusFilter = "" | InvoiceStatus;

export type InvoiceFilters = {
  /** Free-text number prefix, trimmed (max 100 chars). */
  q: string;
  /** Empty means all statuses. */
  status: InvoiceStatusFilter;
  /** 1-based; always >= 1. */
  page: number;
  /** Clamped into [1, MAX_PAGE_SIZE]; default DEFAULT_PAGE_SIZE. */
  pageSize: number;
};

export const DEFAULT_PAGE_SIZE = 20;
export const MAX_PAGE_SIZE = 100;
export const PAGE_SIZE_OPTIONS = [20, 50, 100] as const;
const MAX_PAGE = 1_000_000;
const MAX_QUERY_LENGTH = 100;

/** parseFilters reads the canonical filter state from URL query params. */
export function parseFilters(params: URLSearchParams): InvoiceFilters {
  const q = (params.get("q") ?? "").trim().slice(0, MAX_QUERY_LENGTH);
  const rawStatus = (params.get("status") ?? "").trim();
  const status: InvoiceStatusFilter = (INVOICE_STATUSES as readonly string[]).includes(rawStatus)
    ? (rawStatus as InvoiceStatus)
    : "";
  return {
    q,
    status,
    // Mirrors the server's ValidateList: page < 1 becomes 1; page_size < 1
    // or unparseable becomes the default, > MAX becomes MAX.
    page: clampInt(params.get("page"), 1, MAX_PAGE, 1),
    pageSize: clampInt(params.get("page_size"), 1, MAX_PAGE_SIZE, DEFAULT_PAGE_SIZE, "below-min-fallback"),
  };
}

/**
 * filtersToSearchParams renders the canonical query string: default values
 * (empty q/status, page 1, page size 20) are omitted so one view has one URL.
 */
export function filtersToSearchParams(f: InvoiceFilters): URLSearchParams {
  const params = new URLSearchParams();
  if (f.q !== "") params.set("q", f.q);
  if (f.status !== "") params.set("status", f.status);
  if (f.page > 1) params.set("page", String(f.page));
  if (f.pageSize !== DEFAULT_PAGE_SIZE) params.set("page_size", String(f.pageSize));
  return params;
}

/**
 * withPage applies a page change on top of the current filters (used by the
 * pagination links); filter inputs always navigate from page 1 because the
 * form itself carries no page field.
 */
export function withPage(current: InvoiceFilters, page: number): InvoiceFilters {
  return { ...current, page };
}

function clampInt(
  raw: string | null,
  min: number,
  max: number,
  fallback: number,
  mode: "below-min-fallback" | "below-min-clamp" = "below-min-clamp",
): number {
  const value = Number.parseInt(raw ?? "", 10);
  if (Number.isNaN(value)) return fallback;
  if (value < min) return mode === "below-min-fallback" ? fallback : min;
  return Math.min(value, max);
}
