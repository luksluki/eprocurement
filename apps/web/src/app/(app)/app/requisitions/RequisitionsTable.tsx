"use client";

// RequisitionsTable (issue #32): TanStack Query fetches one server-paginated
// page through the /api/requisitions BFF route (query key = the filter
// tuple); TanStack Table renders it with manual pagination — the client never
// holds more rows than one page. Filters (q/status/page/page_size) live in
// the URL, so reload and back/forward restore the same view; every filter
// change navigates with the page reset to 1.
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
} from "@tanstack/react-table";
import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import type { RequisitionListPage } from "@/lib/gateway";
import {
  PAGE_SIZE_OPTIONS,
  REQUISITION_STATUSES,
  filtersToSearchParams,
  hasActiveFilters,
  parseFilters,
  withFilters,
  type RequisitionFilters,
} from "@/lib/requisition-filters";

class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
  }
}

async function fetchPage(filters: RequisitionFilters): Promise<RequisitionListPage> {
  const query = filtersToSearchParams(filters).toString();
  const res = await fetch(`/api/requisitions${query ? `?${query}` : ""}`);
  if (res.status === 401) {
    throw new UnauthorizedError(); // triggers the refresh dance below
  }
  if (!res.ok) {
    throw new Error(`list failed (${res.status})`);
  }
  return (await res.json()) as RequisitionListPage;
}

const currencyFmt = new Map<string, Intl.NumberFormat>();
function money(cents: number, currency: string): string {
  let fmt = currencyFmt.get(currency);
  if (!fmt) {
    fmt = new Intl.NumberFormat("id-ID", { style: "currency", currency, maximumFractionDigits: 0 });
    currencyFmt.set(currency, fmt);
  }
  return fmt.format(cents / 100);
}

function shortDate(iso: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString("id-ID", { dateStyle: "medium", timeStyle: "short" });
}

const statusStyles: Record<string, string> = {
  draft: "bg-slate-100 text-slate-600",
  submitted: "bg-amber-50 text-amber-700",
  approved: "bg-emerald-50 text-emerald-700",
  rejected: "bg-red-50 text-red-700",
  cancelled: "bg-slate-100 text-slate-400",
  closed: "bg-sky-50 text-sky-700",
};

type Row = RequisitionListPage["requisitions"][number];

const columns: ColumnDef<Row>[] = [
  {
    accessorKey: "number",
    header: "Number",
    // Issue #34: the number opens the requisition detail (header, items,
    // approval steps, attachments, timeline, actions).
    cell: ({ row }) => (
      <Link
        href={`/app/requisitions/${row.original.id}`}
        className="font-medium text-slate-900 underline-offset-2 hover:underline"
        data-testid="requisition-link"
      >
        {row.original.number}
      </Link>
    ),
  },
  {
    accessorKey: "status",
    header: "Status",
    cell: ({ getValue }) => {
      const status = String(getValue());
      return (
        <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${statusStyles[status] ?? "bg-slate-100 text-slate-600"}`}>
          {status}
        </span>
      );
    },
  },
  { accessorKey: "requester_id", header: "Requester", cell: ({ getValue }) => <span className="font-mono text-xs">{String(getValue())}</span> },
  { accessorKey: "currency", header: "Currency" },
  { accessorKey: "total_cents", header: "Total", cell: ({ row }) => money(row.original.total_cents, row.original.currency) },
  { accessorKey: "submitted_at", header: "Submitted", cell: ({ getValue }) => <span className="text-slate-500">{shortDate(String(getValue()))}</span> },
  { accessorKey: "created_at", header: "Created", cell: ({ getValue }) => <span className="text-slate-500">{shortDate(String(getValue()))}</span> },
];

export default function RequisitionsTable() {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const filters = useMemo(() => parseFilters(searchParams), [searchParams]);

  // The URL is the source of truth: every control navigates. Filter changes
  // reset the page to 1 (withFilters); navigation replaces the query string
  // without scrolling so back/forward step through views.
  const navigate = (next: RequisitionFilters) => {
    const query = filtersToSearchParams(next).toString();
    router.push(query ? `${pathname}?${query}` : pathname, { scroll: false });
  };

  const { data, isPending, isError, error, isPlaceholderData, refetch } = useQuery({
    queryKey: ["requisitions", filters],
    queryFn: () => fetchPage(filters),
    placeholderData: keepPreviousData,
  });

  // A 401 from the BFF means the access token expired: run the refresh dance
  // (browser navigation that rotates the cookies and returns to this exact
  // filtered view).
  const unauthorized = error instanceof UnauthorizedError;
  useEffect(() => {
    if (unauthorized) {
      const query = filtersToSearchParams(filters).toString();
      const next = query ? `${pathname}?${query}` : pathname;
      window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(next)}`);
    }
  }, [unauthorized, filters, pathname]);

  const rows = data?.requisitions ?? [];
  const table = useReactTable({
    data: rows,
    columns,
    manualPagination: true, // the server pages; the client renders one page only
    pageCount: data ? Math.max(1, Math.ceil(data.total / data.page_size)) : -1,
    getCoreRowModel: getCoreRowModel(),
    state: { pagination: { pageIndex: filters.page - 1, pageSize: filters.pageSize } },
  });

  const [qInput, setQInput] = useState(filters.q);
  useEffect(() => setQInput(filters.q), [filters.q]);

  const pageCount = data ? Math.max(1, Math.ceil(data.total / data.page_size)) : 1;
  const from = data && data.total > 0 ? (data.page - 1) * data.page_size + 1 : 0;
  const to = data ? Math.min(data.page * data.page_size, data.total) : 0;

  return (
    <div>
      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          navigate(withFilters(filters, { q: qInput.trim() }));
        }}
      >
        <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
          Search number
          <input
            type="search"
            value={qInput}
            onChange={(e) => setQInput(e.target.value)}
            placeholder="PR-000123"
            className="w-48 rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
            data-testid="filter-q"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
          Status
          <select
            value={filters.status}
            onChange={(e) => navigate(withFilters(filters, { status: e.target.value as RequisitionFilters["status"] }))}
            className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-900"
            data-testid="filter-status"
          >
            <option value="">All statuses</option>
            {REQUISITION_STATUSES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
          Page size
          <select
            value={filters.pageSize}
            onChange={(e) => navigate(withFilters(filters, { pageSize: Number(e.target.value) }))}
            className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-900"
            data-testid="filter-page-size"
          >
            {PAGE_SIZE_OPTIONS.map((n) => (
              <option key={n} value={n}>
                {n} / page
              </option>
            ))}
          </select>
        </label>
        <button
          type="submit"
          className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700"
          data-testid="filter-apply"
        >
          Apply
        </button>
      </form>

      <div className="mt-4 overflow-x-auto rounded-xl border border-slate-200 bg-white" data-testid="requisitions-table">
        {isPending ? (
          <div className="px-6 py-10 text-center text-sm text-slate-500" data-testid="state-loading" role="status">
            Loading requisitions…
          </div>
        ) : isError ? (
          <div className="px-6 py-10 text-center text-sm text-red-600" role="alert" data-testid="state-error">
            {unauthorized ? (
              <p>Session expired — redirecting to sign-in…</p>
            ) : (
              <div className="flex flex-col items-center gap-2">
                <p>Failed to load requisitions — the gateway may be unreachable.</p>
                <button
                  type="button"
                  onClick={() => void refetch()}
                  className="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50"
                  data-testid="retry"
                >
                  Retry
                </button>
              </div>
            )}
          </div>
        ) : rows.length === 0 ? (
          hasActiveFilters(filters) ? (
            <div className="flex flex-col items-center gap-2 px-6 py-10 text-center text-sm text-slate-500" data-testid="state-no-results">
              <p>No requisitions match the current filters.</p>
              <button
                type="button"
                onClick={() => navigate({ q: "", status: "", page: 1, pageSize: filters.pageSize })}
                className="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50"
                data-testid="reset-filters"
              >
                Reset filters
              </button>
            </div>
          ) : (
            <div className="px-6 py-10 text-center text-sm text-slate-500" data-testid="state-empty">
              No requisitions yet. See “Demo tenant admins” in docs/onboarding.md for creating the first one via
              POST /api/v1/requisitions.
            </div>
          )
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
              {table.getHeaderGroups().map((hg) => (
                <tr key={hg.id}>
                  {hg.headers.map((h) => (
                    <th key={h.id} className="px-4 py-3 font-medium">
                      {flexRender(h.column.columnDef.header, h.getContext())}
                    </th>
                  ))}
                </tr>
              ))}
            </thead>
            <tbody className={isPlaceholderData ? "opacity-60 transition-opacity" : "transition-opacity"}>
              {table.getRowModel().rows.map((row) => (
                <tr key={row.id} className="border-b border-slate-100 last:border-0">
                  {row.getVisibleCells().map((cell) => (
                    <td key={cell.id} className="px-4 py-3">
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {data && data.total > 0 ? (
        <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-sm text-slate-500" data-testid="pagination">
          <span>
            {from}–{to} of {data.total.toLocaleString("en-US")}
          </span>
          <div className="flex items-center gap-2">
            <button
              type="button"
              disabled={filters.page <= 1}
              onClick={() => navigate({ ...filters, page: filters.page - 1 })}
              className="rounded-lg border border-slate-300 px-3 py-1.5 font-medium text-slate-700 disabled:cursor-not-allowed disabled:opacity-40"
              data-testid="prev-page"
            >
              ← Prev
            </button>
            <span>
              Page {data.page} of {pageCount}
            </span>
            <button
              type="button"
              disabled={filters.page >= pageCount}
              onClick={() => navigate({ ...filters, page: filters.page + 1 })}
              className="rounded-lg border border-slate-300 px-3 py-1.5 font-medium text-slate-700 disabled:cursor-not-allowed disabled:opacity-40"
              data-testid="next-page"
            >
              Next →
            </button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
