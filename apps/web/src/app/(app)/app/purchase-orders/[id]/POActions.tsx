"use client";

// Issue / acknowledge actions for the PO detail page (issue #38). The page
// computes eligibility server-side with the pure helper (lib/po-eligibility,
// mirroring the #36 domain gates) and passes one action at a time — null
// renders nothing (AC "actions hidden when not eligible"); the server keeps
// enforcing every gate, so a lost race (another buyer issued first) surfaces
// as a mapped machine-code message (po_illegal_transition) rather than
// silent success. Success triggers router.refresh() so the server-rendered
// header/items re-render with the new status.
import { useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { callApi } from "@/lib/api-client";

export type POActionKind = "issue" | "acknowledge";

export type POActionsCopy = {
  issue: string;
  acknowledge: string;
  submitting: string;
  genericError: string;
  sessionExpired: string;
  errorMessages: Record<string, string>;
};

export default function POActions({
  poId,
  kind,
  copy,
}: {
  poId: string;
  kind: POActionKind | null;
  copy: POActionsCopy;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  if (!kind) {
    return null; // hidden when not eligible — the helper decided upstream
  }

  async function act() {
    setPending(true);
    setError(null);
    const result = await callApi(`/api/purchase-orders/${encodeURIComponent(poId)}/${kind}`, { method: "POST" });
    setPending(false);
    if (result.ok) {
      router.refresh(); // header/items are server-rendered — re-render them
      return;
    }
    if (result.status === 401) {
      setError(copy.sessionExpired); // visible while the navigation lands
      window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(pathname)}`);
      return;
    }
    setError(copy.errorMessages[result.code] ?? copy.genericError);
  }

  return (
    <div className="flex flex-col items-end gap-2" data-testid="po-actions">
      <button
        type="button"
        disabled={pending}
        onClick={() => void act()}
        className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-50"
        data-testid={`po-${kind}`}
      >
        {kind === "issue" ? copy.issue : copy.acknowledge}
      </button>
      {pending ? (
        <span className="text-xs text-slate-500" data-testid="po-pending">
          {copy.submitting}
        </span>
      ) : null}
      {error ? (
        <p className="text-sm text-red-600" role="alert" data-testid="po-action-error">
          {error}
        </p>
      ) : null}
    </div>
  );
}
