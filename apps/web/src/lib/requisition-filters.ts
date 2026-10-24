// URL filter model for the requisition list (issue #32). The URL is the
// single source of truth: filters live in the query string, so reload and
// back/forward restore the same view, and every filter change navigates with
// page reset to 1. Values are canonicalized here so an equivalent view always
// maps to one URL (defaults are omitted) — mirroring the server-side clamping
// in services/procurement/requisition.ValidateList.

export const REQUISITION_STATUSES = [
  "draft",
  "submitted",
  "approved",
  "rejected",
  "cancelled",
  "closed",
] as const;

export type RequisitionStatus = (typeof REQUISITION_STATUSES)[number];
export type RequisitionStatusFilter = "" | RequisitionStatus;

export type RequisitionFilters = {
  /** Free-text number prefix, trimmed (max 100 chars). */
  q: string;
  /** Empty means all statuses. */
  status: RequisitionStatusFilter;
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
export function parseFilters(params: URLSearchParams): RequisitionFilters {
  const q = (params.get("q") ?? "").trim().slice(0, MAX_QUERY_LENGTH);
  const rawStatus = (params.get("status") ?? "").trim();
  const status: RequisitionStatusFilter = (REQUISITION_STATUSES as readonly string[]).includes(rawStatus)
    ? (rawStatus as RequisitionStatus)
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
export function filtersToSearchParams(f: RequisitionFilters): URLSearchParams {
  const params = new URLSearchParams();
  if (f.q !== "") params.set("q", f.q);
  if (f.status !== "") params.set("status", f.status);
  if (f.page > 1) params.set("page", String(f.page));
  if (f.pageSize !== DEFAULT_PAGE_SIZE) params.set("page_size", String(f.pageSize));
  return params;
}

/**
 * withFilters applies a filter change and resets the page to 1 (any new
 * filter should start from the first page); page-only changes keep the reset.
 */
export function withFilters(
  current: RequisitionFilters,
  patch: Partial<Pick<RequisitionFilters, "q" | "status" | "pageSize">>,
): RequisitionFilters {
  return { ...current, ...patch, page: 1 };
}

/** hasActiveFilters reports whether any user filter (q/status) is set. */
export function hasActiveFilters(f: RequisitionFilters): boolean {
  return f.q !== "" || f.status !== "";
}

function clampInt(raw: string | null, min: number, max: number, fallback: number, mode: "below-min-fallback" | "below-min-clamp" = "below-min-clamp"): number {
  const value = Number.parseInt(raw ?? "", 10);
  if (Number.isNaN(value)) return fallback;
  if (value < min) return mode === "below-min-fallback" ? fallback : min;
  return Math.min(value, max);
}
