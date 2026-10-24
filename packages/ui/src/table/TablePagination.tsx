import type { ComponentType, ReactNode } from "react";

export type PageLinkProps = {
  href: string;
  className?: string;
  rel?: string;
  prefetch?: boolean;
  "aria-label"?: string;
  children: ReactNode;
};

// Next.js's <Link> satisfies this shape; passing it lets the app keep client
// routing while this package stays free of a next dependency. Defaults to a
// plain anchor when no link component is provided.
export type PageLinkComponent = ComponentType<PageLinkProps>;

export type PaginationSummaryRange = {
  from: number;
  to: number;
  total: number;
  page: number;
  pageCount: number;
};

export type PaginationLabels = {
  nav: string;
  previous: string;
  next: string;
  summary: (range: PaginationSummaryRange) => string;
};

export type TablePaginationProps = {
  page: number;
  pageSize: number;
  total: number;
  hrefFor: (page: number) => string;
  link?: PageLinkComponent;
  labels: PaginationLabels;
};

const LINK_CLASSES =
  "focus-ring rounded-md px-3 py-1 text-sm font-medium text-ink-muted hover:bg-canvas";
const DISABLED_CLASSES =
  "cursor-not-allowed rounded-md px-3 py-1 text-sm font-medium text-ink-subtle opacity-50";

export function pageCountOf(total: number, pageSize: number): number {
  return Math.max(1, Math.ceil(total / Math.max(1, pageSize)));
}

// Server-mode pagination controls: the caller renders one page slice and
// computes hrefs; this component only renders summary + prev/next links.
// Edge pages render disabled non-interactive spans (aria-disabled) so the
// control keeps its shape on first/last pages.
// Plain-anchor fallback so the component works without a router link.
const AnchorLink: PageLinkComponent = (props) => <a {...props} />;

export function TablePagination({ page, pageSize, total, hrefFor, link, labels }: TablePaginationProps) {
  const Link: PageLinkComponent = link ?? AnchorLink;
  const pageCount = pageCountOf(total, pageSize);
  const from = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const to = Math.min(total, page * pageSize);

  return (
    <nav aria-label={labels.nav} className="flex items-center justify-between gap-4">
      <p className="text-sm text-ink-subtle">{labels.summary({ from, to, total, page, pageCount })}</p>
      <div className="flex items-center gap-2">
        {page > 1 ? (
          <Link href={hrefFor(page - 1)} rel="prev" className={LINK_CLASSES}>
            {labels.previous}
          </Link>
        ) : (
          <span aria-disabled="true" className={DISABLED_CLASSES}>
            {labels.previous}
          </span>
        )}
        {page < pageCount ? (
          <Link href={hrefFor(page + 1)} rel="next" className={LINK_CLASSES}>
            {labels.next}
          </Link>
        ) : (
          <span aria-disabled="true" className={DISABLED_CLASSES}>
            {labels.next}
          </span>
        )}
      </div>
    </nav>
  );
}
