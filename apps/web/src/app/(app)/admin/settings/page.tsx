import PlaceholderPage from "../../PlaceholderPage";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

export default async function AdminSettingsPage() {
  const { claims } = await requireSession("/admin/settings");

  if (!claims.roles.includes("admin")) {
    return (
      <section>
        <h1 className="text-2xl font-semibold">Settings</h1>
        <p role="alert" className="mt-4 rounded-md bg-red-50 px-3 py-2 text-sm text-red-700">
          Administrators only.
        </p>
      </section>
    );
  }

  return (
    <PlaceholderPage
      title="Settings"
      description="Approval rules and tolerances configuration."
      milestone="the approval configuration milestone"
    />
  );
}
