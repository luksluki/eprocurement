import PlaceholderPage from "../../PlaceholderPage";
import { ErrorState, PageHeader } from "@eprocurement/ui";
import { getDictionary } from "@/i18n/server";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// Admin group (PRD §11 /admin). The sidebar renders this group only for
// admins; the page itself also gates on the claim as defense in depth — the
// gateway remains the real enforcement point (users:manage is admin-only in
// the RBAC matrix).
export default async function AdminUsersPage() {
  const { claims } = await requireSession("/admin/users");
  const dict = await getDictionary();

  if (!claims.roles.includes("admin")) {
    return (
      <section aria-labelledby="users-title">
        <PageHeader id="users-title" title={dict.pages.users.title} />
        <div className="mt-4">
          <ErrorState message={dict.adminGate.message} />
        </div>
      </section>
    );
  }

  const p = dict.pages.users;
  return <PlaceholderPage title={p.title} description={p.description} milestone={p.milestone} />;
}
