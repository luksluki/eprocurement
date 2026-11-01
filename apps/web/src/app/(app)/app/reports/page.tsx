import { Suspense } from "react";
import { requireSession } from "@/lib/session";
import { getDictionary } from "@/i18n/server";
import ExportRunsTable from "./ExportRunsTable";

export const dynamic = "force-dynamic";

// PRD §11: /app/reports — exports, scheduled jobs, and run history (#49
// read side, #53 manual trigger). The server component only guards the
// session (401/expiry runs the refresh dance) and derives the trigger
// visibility from the claims: export:trigger is admin-only (docs/rbac.md —
// the SidebarNav admin-gating pattern; the client-side hide is cosmetic,
// the gateway and procurement enforce the permission). The interactive
// table is a client component fetching the run history through the
// /api/exports/runs BFF route. The nightly export itself is worker-owned
// (Redis lock, missed-run detection); manual runs arrive as
// export.requested events the worker's exports consumer runs.
export default async function ReportsPage() {
  const { claims } = await requireSession("/app/reports");
  const dict = await getDictionary();

  return (
    <section aria-labelledby="reports-title">
      <h1 id="reports-title" className="text-2xl font-semibold">
        {dict.pages.reports.title}
      </h1>
      <p className="mt-1 max-w-3xl text-sm text-slate-500">{dict.pages.reports.description}</p>
      <div className="mt-6">
        <Suspense fallback={null}>
          <ExportRunsTable copy={dict.reportsTable} canTrigger={claims.roles.includes("admin")} />
        </Suspense>
      </div>
    </section>
  );
}
