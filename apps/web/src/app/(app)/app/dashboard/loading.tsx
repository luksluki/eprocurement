import { getDictionary } from "@/i18n/server";

// The /app/dashboard route's loading state (Next.js App Router convention):
// rendered while the page's server data (GET /api/v1/dashboard/summary)
// resolves, on first navigation and on every refresh. Same skeleton shape as
// the page — KPI cards and the widget panels — so the layout does not jump.
// The dictionary is a cookie read (no gateway round trip), so the shell can
// await it and stay locale-correct without waiting for the summary.
export default async function DashboardLoading() {
  const dict = await getDictionary();
  return (
    <section aria-busy="true" aria-live="polite" aria-labelledby="dashboard-loading-title">
      <div className="flex items-baseline justify-between">
        <h1 id="dashboard-loading-title" className="text-xl font-semibold text-ink">
          {dict.dashboard.title}
        </h1>
      </div>
      <div className="mt-6 grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="h-24 animate-pulse rounded-lg bg-surface" />
        ))}
      </div>
      <div className="mt-8 h-64 animate-pulse rounded-lg bg-surface" />
      <div className="mt-8 grid grid-cols-1 gap-8 xl:grid-cols-2">
        <div className="h-64 animate-pulse rounded-lg bg-surface" />
        <div className="h-64 animate-pulse rounded-lg bg-surface" />
      </div>
    </section>
  );
}
