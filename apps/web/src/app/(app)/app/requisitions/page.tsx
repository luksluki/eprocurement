import { Suspense } from "react";
import Link from "next/link";
import { requireSession } from "@/lib/session";
import { getDictionary } from "@/i18n/server";
import RequisitionsTable from "./RequisitionsTable";

export const dynamic = "force-dynamic";

// PRD §11: /app/requisitions — list + filters + pagination (issue #32), plus
// the entry point to the multi-step create form (issue #33). The server
// component only guards the session (401/expiry runs the refresh dance); the
// interactive table is a client component reading its filters from the URL
// (reload/back/forward restore the same view) and fetching one
// server-paginated page at a time through the /api/requisitions BFF route.
export default async function RequisitionsPage() {
  await requireSession("/app/requisitions");
  const dict = await getDictionary();

  return (
    <section aria-labelledby="requisitions-title">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <h1 id="requisitions-title" className="text-2xl font-semibold">
          {dict.pages.requisitions.title}
        </h1>
        <Link
          href="/app/requisitions/new"
          className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700"
          data-testid="new-requisition-link"
        >
          {dict.requisitionForm.newLink}
        </Link>
      </div>
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
