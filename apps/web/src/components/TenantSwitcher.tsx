"use client";

import { useState } from "react";
import { Field } from "@eprocurement/ui";
import type { DemoTenant } from "@/lib/demo-tenants";

export type TenantSwitcherLabels = {
  switchTenant: string;
  switchPlaceholder: string;
  switchingTenant: string;
  switchFailed: string;
  switchNetworkError: string;
};

// Demo tenant switcher (maintainer decision 2026-10-23 on issue #50): POSTs
// to /api/auth/switch-tenant, which re-logs in as the target tenant's admin
// server-side (DEMO_ADMIN_PASSWORD) and rotates the cookies; the full page
// navigation reloads every list scoped to the new tenant. Label + select
// markup come from the design system's Field (control="select").
export default function TenantSwitcher({ tenants, labels }: { tenants: DemoTenant[]; labels: TenantSwitcherLabels }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function switchTo(slug: string) {
    setBusy(true);
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
      setError(data.error ?? labels.switchFailed);
    } catch {
      setError(labels.switchNetworkError);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex items-center gap-2">
      <Field
        id="tenant-switcher"
        control="select"
        label={labels.switchTenant}
        value=""
        placeholderOption={labels.switchPlaceholder}
        options={tenants.map((tenant) => ({ value: tenant.slug, label: tenant.label }))}
        disabled={busy}
        onValueChange={(value) => {
          if (value) void switchTo(value);
        }}
      />
      {busy ? <span className="text-xs text-ink-subtle">{labels.switchingTenant}</span> : null}
      {error ? (
        <span role="alert" className="text-xs text-danger-fg">
          {error}
        </span>
      ) : null}
    </div>
  );
}
