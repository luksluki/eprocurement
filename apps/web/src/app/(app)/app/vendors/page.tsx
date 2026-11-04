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
import { getDictionary } from "@/i18n/server";
import { listVendors, type Vendor } from "@/lib/gateway";
import { parsePageParams } from "@/lib/pagination";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// PRD §11: vendors list. A server component rendering one page slice of
// GET /api/v1/vendors (issue #27 contract: {vendors,total,page,page_size})
// through the design-system DataTable (issue #51); pagination is URL-driven
// (?page=&page_size=) via links, so the table scales by server mode — the
// page slice, not the tenant's full list, is what gets rendered.
export default async function VendorsPage({
  searchParams,
}: {
  searchParams: Promise<{ page?: string; page_size?: string }>;
}) {
  const params = await searchParams;
  const { page, pageSize } = parsePageParams(params);
  const { accessToken } = await requireSession("/app/vendors");
  const dict = await getDictionary();
  const t = dict.vendors;
  const result = await listVendors(accessToken, { page, pageSize });

  const columns: DataTableColumn<Vendor>[] = [
    {
      key: "code",
      label: t.columns.code,
      render: (vendor) => <span className="font-mono text-xs">{vendor.code}</span>,
    },
    { key: "name", label: t.columns.name },
    {
      key: "contact_email",
      label: t.columns.contact,
      render: (vendor) => <span className="text-ink-subtle">{vendor.contact_email}</span>,
    },
    {
      key: "status",
      label: t.columns.status,
      render: (vendor) => (
        <Badge tone={vendor.status === "active" ? "success" : "neutral"}>{vendor.status}</Badge>
      ),
    },
  ];

  const hrefFor = (next: number) => `/app/vendors?page=${next}&page_size=${pageSize}`;

  return (
    <section aria-labelledby="vendors-title">
      <PageHeader id="vendors-title" title={t.title} description={t.description} />
      <div className="mt-6 flex flex-col gap-3">
        {result === null ? (
          <ErrorState message={t.unavailable} />
        ) : (
          <>
            <DataTable
              data={result.vendors}
              columns={columns}
              getRowId={(vendor) => vendor.id}
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
