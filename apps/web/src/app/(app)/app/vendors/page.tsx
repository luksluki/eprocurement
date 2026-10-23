import { listVendors } from "@/lib/gateway";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// PRD §11: vendors list + detail + documents. This server component renders
// the tenant-scoped list from GET /api/v1/vendors (issue #27) — the tenant
// switcher reloads it with the switched tenant's data (acceptance criterion
// "tenant switch reloads scoped data"). Interactive search/filter/table
// (TanStack) arrive with the vendor management screens.
export default async function VendorsPage() {
  const { accessToken } = await requireSession("/app/vendors");
  const vendors = await listVendors(accessToken);

  return (
    <section aria-labelledby="vendors-title">
      <h1 id="vendors-title" className="text-2xl font-semibold">
        Vendors
      </h1>
      <p className="mt-1 text-sm text-slate-500">
        Tenant-scoped vendor master ({vendors === null ? "unavailable" : `${vendors.length} shown, up to 50`}). Cross-tenant vendors are
        never visible.
      </p>
      <div className="mt-6 overflow-x-auto rounded-xl border border-slate-200 bg-white">
        {vendors === null ? (
          <div className="px-6 py-10 text-center text-sm text-red-600" role="alert">
            Failed to load vendors — is the gateway running and the access token valid?
          </div>
        ) : vendors.length === 0 ? (
          <div className="px-6 py-10 text-center text-sm text-slate-500">
            No vendors yet. See “Demo tenant admins” in docs/onboarding.md for seeding vendor data per tenant.
          </div>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
              <tr>
                <th className="px-4 py-3 font-medium">Code</th>
                <th className="px-4 py-3 font-medium">Name</th>
                <th className="px-4 py-3 font-medium">Contact</th>
                <th className="px-4 py-3 font-medium">Status</th>
              </tr>
            </thead>
            <tbody>
              {vendors.map((vendor) => (
                <tr key={vendor.id} className="border-b border-slate-100 last:border-0">
                  <td className="px-4 py-3 font-mono text-xs">{vendor.code}</td>
                  <td className="px-4 py-3">{vendor.name}</td>
                  <td className="px-4 py-3 text-slate-500">{vendor.contact_email}</td>
                  <td className="px-4 py-3">
                    <span
                      className={`rounded-full px-2 py-0.5 text-xs ${
                        vendor.status === "active" ? "bg-emerald-50 text-emerald-700" : "bg-slate-100 text-slate-500"
                      }`}
                    >
                      {vendor.status}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </section>
  );
}
