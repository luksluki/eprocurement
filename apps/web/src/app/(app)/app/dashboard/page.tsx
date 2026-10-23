import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// PRD §11: KPI cards, pending approvals, spend chart. This issue ships the
// shell; the live dashboard data (GET /api/v1/dashboard/summary) arrives with
// the analytics milestone.
export default async function DashboardPage() {
  const { claims } = await requireSession("/app/dashboard");

  return (
    <section aria-labelledby="dashboard-title">
      <div className="flex items-baseline justify-between">
        <h1 id="dashboard-title" className="text-2xl font-semibold">
          Dashboard
        </h1>
        <p className="text-xs text-slate-500">Tenant {claims.tenantId}</p>
      </div>
      <div className="mt-6 grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <KpiCard label="Open requisitions" value="—" note="arrives with the requisition UI" />
        <KpiCard label="Pending approvals" value="—" note="arrives with the approval queue" />
        <KpiCard label="Active purchase orders" value="—" note="arrives with the PO screens" />
        <KpiCard label="Spend this month" value="—" note="arrives with reports" />
      </div>
      <div className="mt-8 rounded-xl border border-dashed border-slate-300 bg-white px-6 py-10 text-center">
        <p className="text-sm font-medium text-slate-700">Spend chart and pending-approval list land with the analytics milestone.</p>
      </div>
    </section>
  );
}

function KpiCard({ label, value, note }: { label: string; value: string; note: string }) {
  return (
    <div className="rounded-xl border border-slate-200 bg-white p-4">
      <p className="text-xs font-medium uppercase tracking-wide text-slate-400">{label}</p>
      <p className="mt-2 text-2xl font-semibold">{value}</p>
      <p className="mt-1 text-xs text-slate-400">{note}</p>
    </div>
  );
}
