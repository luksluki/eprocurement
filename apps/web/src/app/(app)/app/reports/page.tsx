import PlaceholderPage from "../../PlaceholderPage";

export const dynamic = "force-dynamic";

export default function ReportsPage() {
  return (
    <PlaceholderPage
      title="Reports"
      description="Exports, scheduled jobs, and run history (nightly CSV export per tenant to MinIO)."
      milestone="M4 — exports & jobs"
    />
  );
}
