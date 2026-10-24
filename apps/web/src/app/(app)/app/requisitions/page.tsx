import { Suspense } from "react";
import { requireSession } from "@/lib/session";
import RequisitionsTable from "./RequisitionsTable";

export const dynamic = "force-dynamic";

// PRD §11: /app/requisitions — list + filters + pagination (issue #32). The
// server component only guards the session (401/expiry runs the refresh
// dance); the interactive table is a client component reading its filters
// from the URL (reload/back/forward restore the same view) and fetching one
// server-paginated page at a time through the /api/requisitions BFF route.
export default async function RequisitionsPage() {
  await requireSession("/app/requisitions");

  return (
    <section aria-labelledby="requisitions-title">
      <h1 id="requisitions-title" className="text-2xl font-semibold">
        Requisitions
      </h1>
      <p className="mt-1 text-sm text-slate-500">
        Purchase requisitions of your tenant, newest first. Filters and pagination live in the URL and are served
        one page at a time — the full list never loads in the browser.
      </p>
      <div className="mt-6">
        <Suspense fallback={null}>
          <RequisitionsTable />
        </Suspense>
      </div>
    </section>
  );
}
