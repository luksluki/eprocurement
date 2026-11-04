// URL pagination params for the vendors table (server mode). Mirrors the
// procurement service's ValidateList bounds (services/procurement/vendor/
// vendor.go: DefaultPageSize=20, MaxPageSize=100, MaxPage=1000000) so the app
// never asks the gateway for a page it would reject.

export const DEFAULT_PAGE_SIZE = 20;
export const MAX_PAGE_SIZE = 100;
export const MAX_PAGE = 1_000_000;

export type PageParams = {
  page: number;
  pageSize: number;
};

export function parsePageParams(raw: { page?: string; page_size?: string }): PageParams {
  const pageSize = Number.parseInt(raw.page_size ?? "", 10);
  return {
    page: clampInt(raw.page, 1, 1, MAX_PAGE),
    // Procurement semantics: page_size < 1 falls back to the default (not
    // clamped to 1), values above 100 clamp to MaxPageSize.
    pageSize:
      !Number.isFinite(pageSize) || pageSize < 1
        ? DEFAULT_PAGE_SIZE
        : Math.min(MAX_PAGE_SIZE, pageSize),
  };
}

function clampInt(raw: string | undefined, fallback: number, min: number, max: number): number {
  const parsed = Number.parseInt(raw ?? "", 10);
  if (!Number.isFinite(parsed)) {
    return fallback;
  }
  return Math.min(max, Math.max(min, parsed));
}
