"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

export type NavItem = {
  href: string;
  label: string;
};

export type SidebarNavLabels = {
  ariaLabel: string;
  admin: string;
};

// PRD §11 information architecture. Items and copy arrive from the server
// layout (dictionary-driven); the Admin group renders only for admins
// (RoleAdmin, internal/platform/rbac).
export default function SidebarNav({
  isAdmin,
  mainNav,
  adminNav,
  labels,
}: {
  isAdmin: boolean;
  mainNav: NavItem[];
  adminNav: NavItem[];
  labels: SidebarNavLabels;
}) {
  const pathname = usePathname();

  return (
    <nav aria-label={labels.ariaLabel} className="flex flex-1 flex-col gap-6 px-3 py-4">
      <NavGroup items={mainNav} pathname={pathname} />
      {isAdmin ? (
        <div>
          <p className="px-2 pb-1 text-xs font-semibold uppercase tracking-wide text-slate-600">{labels.admin}</p>
          <NavGroup items={adminNav} pathname={pathname} />
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
