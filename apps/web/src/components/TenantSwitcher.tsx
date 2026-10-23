"use client";

import { useState } from "react";
import type { DemoTenant } from "@/lib/demo-tenants";

// Demo tenant switcher (maintainer decision 2026-10-23 on issue #50): POSTs
// to /api/auth/switch-tenant, which re-logs in as the target tenant's admin
// server-side (DEMO_ADMIN_PASSWORD) and rotates the cookies; the full page
// navigation reloads every list scoped to the new tenant.
export default function TenantSwitcher({ tenants }: { tenants: DemoTenant[] }) {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState("");

  async function switchTo(slug: string) {
    setBusy(slug);
    setError("");
    try {
      const res = await fetch(
        `/api/auth/switch-tenant?next=${encodeURIComponent(window.location.pathname)}`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ tenant: slug }),
        },
      );
      const data = (await res.json().catch(() => ({}))) as { redirect?: string; error?: string };
      if (data.redirect) {
        window.location.assign(data.redirect);
        return;
      }
      setError(data.error ?? "Switch failed.");
    } catch {
      setError("Network error during tenant switch.");
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="flex items-center gap-2">
      <label htmlFor="tenant-switcher" className="text-xs text-slate-500">
        Tenant
      </label>
      <select
        id="tenant-switcher"
        className="rounded-md border border-slate-300 bg-white px-2 py-1 text-sm"
        defaultValue=""
        disabled={busy !== null}
        onChange={(e) => {
          if (e.target.value) void switchTo(e.target.value);
        }}
      >
        <option value="" disabled>
          Switch…
        </option>
        {tenants.map((tenant) => (
          <option key={tenant.slug} value={tenant.slug}>
            {tenant.label}
          </option>
        ))}
      </select>
      {busy ? <span className="text-xs text-slate-400">Switching…</span> : null}
      {error ? (
        <span role="alert" className="text-xs text-red-600">
          {error}
        </span>
      ) : null}
    </div>
  );
}
