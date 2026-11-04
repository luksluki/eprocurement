"use client";

// Convert-to-PO action for the requisition detail page (issue #112). The
// page computes eligibility server-side with the pure helper
// (canCreatePOFromRequisition in lib/po-eligibility — po:write holder on an
// approved requisition) and decides rendering; the gateway re-checks the
// permission and procurement re-checks the status/vendor gates (#35), so a
// lost race surfaces as a mapped machine-code message rather than silent
// success. The requisition carries no vendor (PRD §13), so the active-vendor
// picker is part of the action: the page loads the options server-side
// (listVendors with status=active) and passes them down; null means the list
// failed to load and an empty list means the tenant has no active vendors —
// both render an actionable message instead of a dead form. Success follows
// the #35 contract: 201 = this call created the draft PO, 200 = idempotent
// replay of the existing PO (never an error) — both show a confirmation with
// the PO number and a link to the draft, then router.refresh() re-renders
// the server-rendered header (the requisition is closed and this action
// disappears).
import { useState } from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { callApi } from "@/lib/api-client";
import type { PurchaseOrderDetail } from "@/lib/gateway";

export type ConvertToPOVendor = {
  id: string;
  code: string;
  name: string;
};

export type ConvertToPOCopy = {
  action: string;
  vendorLabel: string;
  vendorPlaceholder: string;
  submitting: string;
  noVendors: string;
  loadFailed: string;
  createdTitle: string;
  replayedTitle: string;
  openPo: string;
  genericError: string;
  sessionExpired: string;
  // Machine code → human sentence for the gateway's conversion failures
  // (poMessages in services/gateway/server/purchaseorders.go) plus the BFF's
  // local mirrors (po_vendor_id_invalid, body_invalid).
  errorMessages: Record<string, string>;
};

type Confirmation = { po: PurchaseOrderDetail; created: boolean };

export default function ConvertToPO({
  requisitionId,
  vendors,
  copy,
}: {
  requisitionId: string;
  vendors: ConvertToPOVendor[] | null;
  copy: ConvertToPOCopy;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const [vendorId, setVendorId] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);

  // The vendor list is part of the form's usability, not its gate: a failed
  // load or an empty tenant shows an actionable message instead of a picker
  // that can never succeed (every conversion needs an active vendor, #35).
  if (vendors === null) {
    return (
      <div
        className="rounded-xl border border-slate-200 bg-white p-4 text-sm text-slate-500 print:hidden"
        data-testid="convert-load-failed"
      >
        {copy.loadFailed}
      </div>
    );
  }
  if (vendors.length === 0) {
    return (
      <div
        className="rounded-xl border border-slate-200 bg-white p-4 text-sm text-slate-500 print:hidden"
        data-testid="convert-no-vendors"
      >
        {copy.noVendors}
      </div>
    );
  }

  async function convert() {
    if (vendorId === "") {
      return; // the disabled button already guards this; keep the type honest
    }
    setPending(true);
    setError(null);
    const result = await callApi<PurchaseOrderDetail>(
      `/api/purchase-orders/from-requisition/${encodeURIComponent(requisitionId)}`,
      { method: "POST", body: { vendor_id: vendorId } },
    );
    setPending(false);
    if (result.ok) {
      // The #35 contract line: 201 = this call created the draft, 200 =
      // idempotent replay of the existing PO — both are successes.
      setConfirmation({ po: result.data, created: result.status !== 200 });
      router.refresh(); // the header is server-rendered — the closed status lands
      return;
    }
    if (result.status === 401) {
      setError(copy.sessionExpired); // visible while the navigation lands
      window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(pathname)}`);
      return;
    }
    setError(copy.errorMessages[result.code] ?? copy.genericError);
  }

  if (confirmation) {
    return (
      <div
        className="rounded-xl border border-emerald-200 bg-emerald-50 p-4 print:hidden"
        data-testid="convert-success"
      >
        <p className="text-sm font-medium text-emerald-800" data-testid="convert-success-title">
          {confirmation.created ? copy.createdTitle : copy.replayedTitle}
        </p>
        <p className="mt-1 font-mono text-sm text-emerald-900" data-testid="convert-po-number">
          {confirmation.po.number}
        </p>
        <Link
          href={`/app/purchase-orders/${confirmation.po.id}`}
          className="mt-2 inline-block text-sm font-medium text-emerald-700 underline"
          data-testid="convert-po-link"
        >
          {copy.openPo}
        </Link>
      </div>
    );
  }

  return (
    <div
      className="rounded-xl border border-slate-200 bg-white p-4 print:hidden"
      data-testid="convert-to-po"
    >
      <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
        {copy.vendorLabel}
        <select
          value={vendorId}
          onChange={(e) => setVendorId(e.target.value)}
          className="w-72 rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-900"
          data-testid="convert-vendor"
        >
          <option value="">{copy.vendorPlaceholder}</option>
          {vendors.map((vendor) => (
            <option key={vendor.id} value={vendor.id}>
              {vendor.code} — {vendor.name}
            </option>
          ))}
        </select>
      </label>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <button
          type="button"
          disabled={pending || vendorId === ""}
          onClick={() => void convert()}
          className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-50"
          data-testid="convert-submit"
        >
          {copy.action}
        </button>
        {pending ? (
          <span className="text-xs text-slate-500" data-testid="convert-pending">
            {copy.submitting}
          </span>
        ) : null}
      </div>
      {error ? (
        <p className="mt-2 text-sm text-red-600" role="alert" data-testid="convert-error">
          {error}
        </p>
      ) : null}
    </div>
  );
}
