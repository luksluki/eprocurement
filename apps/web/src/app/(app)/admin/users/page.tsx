import PlaceholderPage from "../../PlaceholderPage";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// Admin group (PRD §11 /admin). The sidebar renders this group only for
// admins; the page itself also gates on the claim as defense in depth — the
// gateway remains the real enforcement point (users:manage is admin-only in
// the RBAC matrix).
export default async function AdminUsersPage() {
  const { claims } = await requireSession("/admin/users");

  if (!claims.roles.includes("admin")) {
    return (
      <section>
        <h1 className="text-2xl font-semibold">Users</h1>
        <p role="alert" className="mt-4 rounded-md bg-red-50 px-3 py-2 text-sm text-red-700">
          Administrators only.
        </p>
      </section>
    );
  }

  return (
    <PlaceholderPage
      title="Users"
      description="Users and roles administration for this tenant."
      milestone="the identity admin milestone"
    />
  );
}
