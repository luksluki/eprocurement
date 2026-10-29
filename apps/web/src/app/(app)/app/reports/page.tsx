import { Suspense } from "react";
import { requireSession } from "@/lib/session";
import { getDictionary } from "@/i18n/server";
import ExportRunsTable from "./ExportRunsTable";

export const dynamic = "force-dynamic";

// PRD §11: /app/reports — exports, scheduled jobs, and run history (#49).
// The server component only guards the session (401/expiry runs the
// refresh dance); the interactive table is a client component fetching the
// run history through the /api/exports/runs BFF route. The nightly export
// itself is worker-owned (Redis lock, missed-run detection); this page is
// its read side — download included, via short-lived presigned URLs.
export default async function ReportsPage() {
  await requireSession("/app/reports");
  const dict = await getDictionary();

  return (
    <section aria-labelledby="reports-title">
      <h1 id="reports-title" className="text-2xl font-semibold">
        {dict.pages.reports.title}
      </h1>
      <p className="mt-1 max-w-3xl text-sm text-slate-500">{dict.pages.reports.description}</p>
      <div className="mt-6">
        <Suspense fallback={null}>
          <ExportRunsTable copy={dict.reportsTable} />
        </Suspense>
      </div>
    </section>
  );
}
