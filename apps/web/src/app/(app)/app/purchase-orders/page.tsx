import Link from "next/link";
import {
  Badge,
  DataTable,
  EmptyState,
  ErrorState,
  PageHeader,
  TablePagination,
  type DataTableColumn,
} from "@eprocurement/ui";
import { getDictionary, getLocale } from "@/i18n/server";
import { listPurchaseOrders, type PurchaseOrderHeader } from "@/lib/gateway";
import {
  PAGE_SIZE_OPTIONS,
  PURCHASE_ORDER_STATUSES,
  filtersToSearchParams,
  parseFilters,
  withPage,
} from "@/lib/purchase-order-filters";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// PRD §11: /app/purchase-orders — list + detail + GRN actions (issue #38).
// A server component rendering one page slice of GET /api/v1/purchase-orders
// through the design-system DataTable (the #27/#51 vendor-list pattern).
// Filters (q/status/page/page_size) live in the URL; the filter controls are
// a plain GET <form> and the page links are anchors, so the screen works
// without client JavaScript. The number column links to the detail page.
export default async function PurchaseOrdersPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const params = await searchParams;
  const flat = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    const first = Array.isArray(value) ? value[0] : value;
    if (first !== undefined) flat.set(key, first);
  }
  const filters = parseFilters(flat);
  const { accessToken } = await requireSession("/app/purchase-orders");
  const [locale, dict] = await Promise.all([getLocale(), getDictionary()]);
  const t = dict.purchaseOrders;
  const result = await listPurchaseOrders(accessToken, {
    q: filters.q,
    status: filters.status,
    page: filters.page,
    pageSize: filters.pageSize,
  });

  const localeTag = locale === "id" ? "id-ID" : "en-US";
  const money = (cents: number, currency: string) =>
    new Intl.NumberFormat(localeTag, { style: "currency", currency, maximumFractionDigits: 0 }).format(cents / 100);
  const dateTime = (iso: string) =>
    iso ? new Intl.DateTimeFormat(localeTag, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso)) : "—";

  // Status badge tones for the seven FR-06 statuses (neutral/default).
  const badgeTone = (status: string): "success" | "danger" | "neutral" => {
    if (status === "received" || status === "closed" || status === "acknowledged") return "success";
    if (status === "cancelled") return "danger";
    return "neutral";
  };

  const columns: DataTableColumn<PurchaseOrderHeader>[] = [
    {
      key: "number",
      label: t.columns.number,
      render: (po) => (
        <Link
          href={`/app/purchase-orders/${po.id}`}
          className="font-medium text-slate-900 underline-offset-2 hover:underline"
          data-testid="purchase-order-link"
        >
          {po.number}
        </Link>
      ),
    },
    {
      key: "status",
      label: t.columns.status,
      render: (po) => <Badge tone={badgeTone(po.status)}>{po.status}</Badge>,
    },
    {
      key: "vendor_id",
      label: t.columns.vendor,
      render: (po) => <span className="font-mono text-xs">{po.vendor_id}</span>,
    },
    { key: "currency", label: t.columns.currency },
    {
      key: "total_cents",
      label: t.columns.total,
      render: (po) => money(po.total_cents, po.currency),
    },
    {
      key: "issued_at",
      label: t.columns.issued,
      render: (po) => <span className="text-ink-subtle">{dateTime(po.issued_at)}</span>,
    },
    {
      key: "created_at",
      label: t.columns.created,
      render: (po) => <span className="text-ink-subtle">{dateTime(po.created_at)}</span>,
    },
  ];

  // Pagination keeps the active filters; hrefFor renders them into the links.
  const hrefFor = (next: number) => {
    const query = filtersToSearchParams(withPage(filters, next)).toString();
    return query ? `/app/purchase-orders?${query}` : "/app/purchase-orders";
  };

  return (
    <section aria-labelledby="purchase-orders-title">
      <PageHeader id="purchase-orders-title" title={t.title} description={t.description} />

      {/* A plain GET form: filters navigate by URL, no client JS required. */}
      <form method="get" action="/app/purchase-orders" className="mt-6 flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
          {t.filters.numberLabel}
          <input
            type="search"
            name="q"
            defaultValue={filters.q}
            placeholder={t.filters.numberPlaceholder}
            className="w-48 rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
            data-testid="filter-q"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
          {t.filters.statusLabel}
          <select
            name="status"
            defaultValue={filters.status}
            className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-900"
            data-testid="filter-status"
          >
            <option value="">{t.filters.statusAll}</option>
            {PURCHASE_ORDER_STATUSES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
          {t.filters.pageSizeLabel}
          <select
            name="page_size"
            defaultValue={String(filters.pageSize)}
            className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-900"
            data-testid="filter-page-size"
          >
            {PAGE_SIZE_OPTIONS.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <button
          type="submit"
          className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700"
          data-testid="filter-apply"
        >
          {t.filters.apply}
        </button>
      </form>

      <div className="mt-4 flex flex-col gap-3">
        {result === null ? (
          <ErrorState message={t.unavailable} />
        ) : (
          <>
            <DataTable
              data={result.purchase_orders}
              columns={columns}
              getRowId={(po) => po.id}
              caption={t.caption}
              emptyState={<EmptyState title={t.empty} body={t.emptyHint} />}
            />
            {result.total > 0 ? (
              <TablePagination
                page={result.page}
                pageSize={result.page_size}
                total={result.total}
                hrefFor={hrefFor}
                link={Link}
                labels={t.pagination}
              />
            ) : null}
          </>
        )}
      </div>
    </section>
  );
}
