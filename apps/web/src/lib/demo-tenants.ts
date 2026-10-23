// Demo tenant switcher config (maintainer decision 2026-10-23 on issue #50):
// the switcher re-logs in as the target tenant's admin using a shared demo
// password from the environment. Entries come from DEMO_TENANTS as
// "slug=email" pairs separated by commas, semicolons, or newlines, e.g.
//
//	DEMO_TENANTS=acme=admin@acme.example,glow=admin@glow.example
//
// Each email must belong to exactly one tenant: identity logins fail with a
// generic 401 when an email matches users in several tenants
// (docs/schema/identity.md).

export type DemoTenant = {
  slug: string;
  label: string;
  email: string;
};

export function parseDemoTenants(raw: string | null | undefined): DemoTenant[] {
  if (!raw) {
    return [];
  }
  const tenants: DemoTenant[] = [];
  for (const entry of raw.split(/[,;\n]/)) {
    const pair = entry.trim();
    if (!pair) {
      continue;
    }
    const eq = pair.indexOf("=");
    if (eq <= 0 || eq === pair.length - 1) {
      continue; // not "slug=email" — skip malformed entries
    }
    const slug = pair.slice(0, eq).trim();
    const email = pair.slice(eq + 1).trim();
    if (!slug || !email.includes("@")) {
      continue;
    }
    tenants.push({ slug, label: slugToLabel(slug), email });
  }
  return tenants;
}

function slugToLabel(slug: string): string {
  return slug
    .split(/[-_]/)
    .filter(Boolean)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(" ");
}

export function demoAdminPassword(): string {
  return process.env.DEMO_ADMIN_PASSWORD ?? "";
}
