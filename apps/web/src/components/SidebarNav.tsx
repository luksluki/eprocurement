"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

type NavItem = {
  href: string;
  label: string;
};

// PRD §11 information architecture. The Admin group renders only for admins
// (RoleAdmin, internal/platform/rbac) — passed in as a prop from the server
// layout, which holds the verified claims.
const MAIN_NAV: NavItem[] = [
  { href: "/app/dashboard", label: "Dashboard" },
  { href: "/app/requisitions", label: "Requisitions" },
  { href: "/app/approvals", label: "Approvals" },
  { href: "/app/purchase-orders", label: "Purchase Orders" },
  { href: "/app/vendors", label: "Vendors" },
  { href: "/app/invoices", label: "Invoices" },
  { href: "/app/reports", label: "Reports" },
];

const ADMIN_NAV: NavItem[] = [
  { href: "/admin/users", label: "Users" },
  { href: "/admin/settings", label: "Settings" },
];

export default function SidebarNav({ isAdmin }: { isAdmin: boolean }) {
  const pathname = usePathname();

  return (
    <nav aria-label="Main navigation" className="flex flex-1 flex-col gap-6 px-3 py-4">
      <NavGroup items={MAIN_NAV} pathname={pathname} />
      {isAdmin ? (
        <div>
          <p className="px-2 pb-1 text-xs font-semibold uppercase tracking-wide text-slate-400">Admin</p>
          <NavGroup items={ADMIN_NAV} pathname={pathname} />
        </div>
      ) : null}
    </nav>
  );
}

function NavGroup({ items, pathname }: { items: NavItem[]; pathname: string }) {
  return (
    <ul className="flex flex-col gap-0.5">
      {items.map((item) => {
        const active = pathname === item.href || pathname.startsWith(`${item.href}/`);
        return (
          <li key={item.href}>
            <Link
              href={item.href}
              aria-current={active ? "page" : undefined}
              className={`block rounded-md px-3 py-1.5 text-sm ${
                active ? "bg-slate-100 font-medium text-slate-900" : "text-slate-600 hover:bg-slate-50 hover:text-slate-900"
              }`}
            >
              {item.label}
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
