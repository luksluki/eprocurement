import PlaceholderPage from "../../PlaceholderPage";
import { ErrorState, PageHeader } from "@eprocurement/ui";
import { getDictionary } from "@/i18n/server";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

export default async function AdminSettingsPage() {
  const { claims } = await requireSession("/admin/settings");
  const dict = await getDictionary();

  if (!claims.roles.includes("admin")) {
    return (
      <section aria-labelledby="settings-title">
        <PageHeader id="settings-title" title={dict.pages.settings.title} />
        <div className="mt-4">
          <ErrorState message={dict.adminGate.message} />
        </div>
      </section>
    );
  }

  const p = dict.pages.settings;
  return <PlaceholderPage title={p.title} description={p.description} milestone={p.milestone} />;
}
