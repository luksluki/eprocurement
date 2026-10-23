import type { ReactNode } from "react";
import LogoutButton from "@/components/LogoutButton";
import SidebarNav from "@/components/SidebarNav";
import TenantSwitcher from "@/components/TenantSwitcher";
import { parseDemoTenants } from "@/lib/demo-tenants";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// Shell layout for the authenticated area (PRD §11): sidebar navigation,
// tenant context, demo tenant switcher, and sign-out. The route group "(app)"
// adds no URL segment — pages live under the literal app/ and admin/
// directories, serving /app/… and /admin/… exactly as the PRD's information
// architecture requires.
//
// Layouts do not know their pathname, so an expired session refresh-dances
// back to /app/dashboard rather than the exact page (page-level
// requireSession calls carry the precise path).
export default async function AppShellLayout({ children }: { children: ReactNode }) {
  const { claims } = await requireSession("/app/dashboard");
  const isAdmin = claims.roles.includes("admin");
  const demoTenants = parseDemoTenants(process.env.DEMO_TENANTS);

  return (
    <div className="flex min-h-screen">
      <aside className="hidden w-60 shrink-0 flex-col border-r border-slate-200 bg-white md:flex">
        <div className="flex items-center gap-2 border-b border-slate-200 px-4 py-4">
          <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-slate-900 text-sm font-bold text-white">
            eP
          </span>
          <span className="font-semibold">eProcurement</span>
        </div>
        <SidebarNav isAdmin={isAdmin} />
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center justify-between gap-4 border-b border-slate-200 bg-white px-6 py-3">
          <div className="min-w-0">
            <p className="truncate text-sm font-medium">Tenant {claims.tenantId}</p>
            <p className="truncate text-xs text-slate-500">
              {claims.userId} · {claims.roles.join(", ") || "no roles"}
            </p>
          </div>
          <div className="flex items-center gap-3">
            {demoTenants.length > 0 ? <TenantSwitcher tenants={demoTenants} /> : null}
            <LogoutButton />
          </div>
        </header>
        <main className="flex-1 px-6 py-8">{children}</main>
      </div>
    </div>
  );
}
