"use client";

// Match action for the invoice detail page (issue #41). The page computes
// eligibility server-side with the pure helper (lib/invoice-eligibility,
// mirroring the #40 re-run guard) and passes the viewer's permission plus
// the attachment facts; the maintainer's hard-gate decision (2026-10-29)
// keeps the button DISABLED until the invoice has an attachment in ready
// state — client-side only, the server stays non-checking per #40 — with the
// gate's reason rendered next to it. Success triggers router.refresh() so
// the server-rendered header/lines/match panel re-render with the verdict;
// 409 inv_already_matched maps inline (a concurrent match won the race).
import { useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { callApi } from "@/lib/api-client";

export type InvoiceActionsCopy = {
  match: string;
  submitting: string;
  gated: string;
  genericError: string;
  sessionExpired: string;
  errorMessages: Record<string, string>;
};

export default function InvoiceActions({
  invoiceId,
  canMatch,
  attachmentReady,
  copy,
}: {
  invoiceId: string;
  canMatch: boolean;
  attachmentReady: boolean;
  copy: InvoiceActionsCopy;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  if (!canMatch) {
    return null; // hidden when not eligible — the helper decided upstream
  }

  // The hard gate: no ready attachment, no match run (maintainer decision
  // 2026-10-29). The server never checks this (#40 decision) — the disabled
  // control carries the demo flow's obligation.
  const gated = !attachmentReady;

  async function runMatch() {
    setPending(true);
    setError(null);
    const result = await callApi(`/api/invoices/${encodeURIComponent(invoiceId)}/match`, { method: "POST" });
    setPending(false);
    if (result.ok) {
      router.refresh(); // header/lines/match panel are server-rendered
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
    <div className="flex flex-col items-end gap-2" data-testid="invoice-actions">
      <button
        type="button"
        disabled={pending || gated}
        onClick={() => void runMatch()}
        className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-50"
        data-testid="invoice-match"
      >
        {copy.match}
      </button>
      {pending ? (
        <span className="text-xs text-slate-500" data-testid="invoice-match-pending">
          {copy.submitting}
        </span>
      ) : null}
      {gated ? (
        <p className="max-w-xs text-right text-xs text-amber-700" data-testid="invoice-match-gate">
          {copy.gated}
        </p>
      ) : null}
      {error ? (
        <p className="text-sm text-red-600" role="alert" data-testid="invoice-match-error">
          {error}
        </p>
      ) : null}
    </div>
  );
}
