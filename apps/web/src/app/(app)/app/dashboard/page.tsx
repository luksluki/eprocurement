import Link from "next/link";
import {
  Badge,
  Card,
  DataTable,
  EmptyState,
  ErrorState,
  PageHeader,
  type DataTableColumn,
} from "@eprocurement/ui";
import { getDictionary, getLocale } from "@/i18n/server";
import {
  getDashboardSummary,
  type PendingApprovalWire,
  type VendorSpendWire,
} from "@/lib/gateway";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// PRD §11 / FR-09: the operational-health landing page — KPI cards, the
// pending-approval queue, spend per vendor (30/90 days), and the PO status
// distribution. A server component over ONE GET /api/v1/dashboard/summary
// round trip (issue #52); the route-level loading.tsx covers navigation and
// refreshes, ErrorState/EmptyState cover the fetch outcome, and the
// requisition number links to the detail view (where the step is decided).
export default async function DashboardPage() {
  const { claims, accessToken } = await requireSession("/app/dashboard");
  const dict = await getDictionary();
  const locale = await getLocale();
  const t = dict.dashboard;
  const summary = await getDashboardSummary(accessToken);

  const localeTag = locale === "id" ? "id-ID" : "en-US";
  const money = (cents: number, currency: string) =>
    new Intl.NumberFormat(localeTag, { style: "currency", currency, maximumFractionDigits: 0 }).format(
      cents / 100,
    );
  const count = (n: number) => n.toLocaleString(localeTag);
  const dateTime = (iso: string) =>
    new Intl.DateTimeFormat(localeTag, { dateStyle: "medium", timeStyle: "short" }).format(
      new Date(iso),
    );

  const poTotal = summary
    ? summary.po_status_distribution.reduce((sum, d) => sum + d.count, 0)
    : 0;
  const kpiCards = [
    { label: t.kpi.pending, value: summary ? count(summary.pending_approvals.total) : "—", note: t.kpi.pendingNote },
    {
      label: t.kpi.spend30,
      value: summary ? spendKpi(summary.spend_totals, "thirty_day_cents", money) : "—",
      note: t.kpi.spend30Note,
    },
    {
      label: t.kpi.spend90,
      value: summary ? spendKpi(summary.spend_totals, "ninety_day_cents", money) : "—",
      note: t.kpi.spend90Note,
    },
    { label: t.kpi.poTotal, value: summary ? count(poTotal) : "—", note: t.kpi.poTotalNote },
  ];

  const approvalColumns: DataTableColumn<PendingApprovalWire>[] = [
    {
      key: "number",
      label: t.approvals.columns.number,
      render: (row) => (
        <Link className="font-mono text-xs underline" href={`/app/requisitions/${row.requisition_id}`}>
          {row.number}
        </Link>
      ),
    },
    {
      key: "submitted_at",
      label: t.approvals.columns.submitted,
      render: (row) => <span className="text-ink-subtle">{dateTime(row.submitted_at)}</span>,
    },
    {
      key: "total_cents",
      label: t.approvals.columns.total,
      render: (row) => money(row.total_cents, row.currency),
      align: "right",
    },
    {
      key: "step_no",
      label: t.approvals.columns.step,
      render: (row) => <span className="font-mono text-xs">{`${row.step_no} (#${row.step_id.slice(0, 8)})`}</span>,
    },
    { key: "role_required", label: t.approvals.columns.role, render: (row) => <Badge>{row.role_required}</Badge> },
  ];

  const spendColumns: DataTableColumn<VendorSpendWire>[] = [
    { key: "vendor_name", label: t.spend.columns.vendor },
    {
      key: "thirty_day_cents",
      label: t.spend.columns.thirty,
      render: (row) => money(row.thirty_day_cents, row.currency),
      align: "right",
    },
    {
      key: "ninety_day_cents",
      label: t.spend.columns.ninety,
      render: (row) => money(row.ninety_day_cents, row.currency),
      align: "right",
    },
  ];

  return (
    <section aria-labelledby="dashboard-title">
      <div className="flex items-baseline justify-between">
        <PageHeader id="dashboard-title" title={t.title} description={t.description} />
        <p className="text-xs text-slate-500">
          {dict.header.tenantLabel} {claims.tenantId}
        </p>
      </div>

      {summary === null ? (
        <div className="mt-6">
          <ErrorState message={t.unavailable} />
        </div>
      ) : (
        <>
          <div className="mt-6 grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {kpiCards.map((kpi) => (
              <KpiCard key={kpi.label} label={kpi.label} value={kpi.value} note={kpi.note} />
            ))}
          </div>

          <Card className="mt-8 p-4">
            <h2 className="text-sm font-semibold text-ink">{t.approvals.heading}</h2>
            <div className="mt-3">
              <DataTable
                data={summary.pending_approvals.items}
                columns={approvalColumns}
                getRowId={(row) => row.step_id}
                caption={t.approvals.caption}
                emptyState={<EmptyState title={t.approvals.empty} />}
              />
              {summary.pending_approvals.total > summary.pending_approvals.items.length ? (
                <p className="mt-2 text-xs text-ink-subtle">
                  {t.approvals.hidden(summary.pending_approvals.total - summary.pending_approvals.items.length)}
                </p>
              ) : null}
            </div>
          </Card>

          <div className="mt-8 grid grid-cols-1 gap-8 xl:grid-cols-2">
            <Card className="p-4">
              <h2 className="text-sm font-semibold text-ink">{t.spend.heading}</h2>
              <div className="mt-3">
                <DataTable
                  data={summary.spend_by_vendor}
                  columns={spendColumns}
                  getRowId={(row) => `${row.vendor_id}-${row.currency}`}
                  caption={t.spend.caption}
                  emptyState={<EmptyState title={t.spend.empty} />}
                />
              </div>
            </Card>

            <Card className="p-4">
              <h2 className="text-sm font-semibold text-ink">{t.distribution.heading}</h2>
              <dl className="mt-3 divide-y divide-line-soft">
                {summary.po_status_distribution.map((d) => (
                  <div key={d.status} className="flex items-center justify-between py-2">
                    <dt>
                      <Badge
                        tone={
                          d.status === "received" || d.status === "closed" || d.status === "acknowledged"
                            ? "success"
                            : d.status === "cancelled"
                              ? "danger"
                              : "neutral"
                        }
                      >
                        {d.status}
                      </Badge>
                    </dt>
                    <dd className="font-mono text-sm text-ink">{count(d.count)}</dd>
                  </div>
                ))}
                <div className="flex items-center justify-between py-2">
                  <dt className="text-xs font-medium uppercase tracking-wide text-ink-subtle">
                    {t.distribution.total}
                  </dt>
                  <dd className="font-mono text-sm font-semibold text-ink">{count(poTotal)}</dd>
                </div>
              </dl>
            </Card>
          </div>
        </>
      )}
    </section>
  );
}

// spendKpi renders the per-currency KPI spend: each currency's unbounded
// window sum on its own line (money never mixes currencies — a tenant may
// hold IDR and USD side by side; the totals arrive largest first).
function spendKpi(
  totals: ReadonlyArray<{ currency: string; thirty_day_cents: number; ninety_day_cents: number }>,
  field: "thirty_day_cents" | "ninety_day_cents",
  money: (cents: number, currency: string) => string,
): string {
  if (totals.length === 0) return "—";
  return totals.map((c) => money(c[field], c.currency)).join(" · ");
}

function KpiCard({ label, value, note }: { label: string; value: string; note: string }) {
  return (
    <Card className="p-4">
      <p className="text-xs font-medium uppercase tracking-wide text-ink-subtle">{label}</p>
      <p className="mt-2 text-2xl font-semibold text-ink">{value}</p>
      <p className="mt-1 text-xs text-ink-subtle">{note}</p>
    </Card>
  );
}
