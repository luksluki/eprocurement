import { Card, EmptyState, PageHeader } from "@eprocurement/ui";
import { getDictionary } from "@/i18n/server";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// PRD §11: KPI cards, pending approvals, spend chart. This issue ships the
// shell; the live dashboard data (GET /api/v1/dashboard/summary) arrives with
// the analytics milestone. KPI labels/notes are dictionary copy (issue #51);
// cards and the dashed panel reuse the design system.
export default async function DashboardPage() {
  const { claims } = await requireSession("/app/dashboard");
  const dict = await getDictionary();

  return (
    <section aria-labelledby="dashboard-title">
      <div className="flex items-baseline justify-between">
        <PageHeader id="dashboard-title" title={dict.dashboard.title} />
        <p className="text-xs text-slate-500">
          {dict.header.tenantLabel} {claims.tenantId}
        </p>
      </div>
      <div className="mt-6 grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {dict.dashboard.kpi.map((kpi) => (
          <KpiCard key={kpi.label} label={kpi.label} value="—" note={kpi.note} />
        ))}
      </div>
      <Card variant="dashed" className="mt-8">
        <EmptyState title={dict.dashboard.chartNote} />
      </Card>
    </section>
  );
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
