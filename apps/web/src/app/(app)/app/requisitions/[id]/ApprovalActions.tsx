"use client";

// Approve/reject actions for the requisition detail page (issue #34). The
// page computes eligibility server-side with the pure helper
// (lib/approval-eligibility) and passes the decidable step — null renders
// nothing (AC "actions hidden when not permitted"); the server keeps
// enforcing every gate, so a lost race (another approver decided first)
// surfaces as a mapped machine-code message rather than silent success.
// The component is client-side only for the interaction: reason validation
// mirrors the edge contract (reject requires a non-blank reason ≤ 1000
// chars, services/gateway/server/approvals.go), a fresh Idempotency-Key per
// attempt (crypto.randomUUID) makes double-clicks harmless, and a success
// triggers router.refresh() so the server-rendered header/steps re-render.
import { useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { callApi } from "@/lib/api-client";

// Matches the gateway's maxReasonLen (services/gateway/server/approvals.go).
const MAX_REASON_LEN = 1000;

export type DecidableStep = {
  id: string;
  step_no: number;
  role_required: string;
};

export type ApprovalActionsCopy = {
  approve: string;
  reject: string;
  reasonLabel: string;
  reasonPlaceholder: string;
  reasonRequired: string;
  reasonTooLong: string;
  submitting: string;
  genericError: string;
  sessionExpired: string;
  // Machine code → human sentence for the gateway's approval failures.
  errorMessages: Record<string, string>;
};

export default function ApprovalActions({
  step,
  copy,
}: {
  step: DecidableStep | null;
  copy: ApprovalActionsCopy;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  if (!step) {
    return null; // hidden when not permitted — the helper decided upstream
  }
  // Parameters are not const, so the narrowing above does not survive into
  // the async closure — pin the non-null step for the decide call.
  const decidable = step;

  async function decide(decision: "approve" | "reject") {
    const trimmed = reason.trim();
    if (decision === "reject" && trimmed === "") {
      setError(copy.reasonRequired);
      return;
    }
    if (trimmed.length > MAX_REASON_LEN) {
      setError(copy.reasonTooLong);
      return;
    }
    setPending(true);
    setError(null);
    // Fresh key per attempt: a retried click after a failure must not replay
    // the failed decision, and a double-click mid-flight hits the gateway's
    // idempotent replay instead of recording two decisions.
    const result = await callApi(`/api/approvals/${encodeURIComponent(decidable.id)}/decide`, {
      method: "POST",
      headers: { "idempotency-key": crypto.randomUUID() },
      body: { decision, reason: trimmed },
    });
    setPending(false);
    if (result.ok) {
      router.refresh(); // header/steps are server-rendered — re-render them
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
    <div
      className="rounded-xl border border-slate-200 bg-white p-4 print:hidden"
      data-testid="approval-actions"
    >
      <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
        {copy.reasonLabel}
        <textarea
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          placeholder={copy.reasonPlaceholder}
          rows={2}
          maxLength={MAX_REASON_LEN}
          className="w-72 rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
          data-testid="approval-reason"
        />
      </label>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <button
          type="button"
          disabled={pending}
          onClick={() => void decide("approve")}
          className="rounded-lg bg-emerald-600 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-500 disabled:cursor-not-allowed disabled:opacity-50"
          data-testid="approval-approve"
        >
          {copy.approve}
        </button>
        <button
          type="button"
          disabled={pending}
          onClick={() => void decide("reject")}
          className="rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-500 disabled:cursor-not-allowed disabled:opacity-50"
          data-testid="approval-reject"
        >
          {copy.reject}
        </button>
        {pending ? (
          <span className="text-xs text-slate-500" data-testid="approval-pending">
            {copy.submitting}
          </span>
        ) : null}
      </div>
      {error ? (
        <p className="mt-2 text-sm text-red-600" role="alert" data-testid="approval-error">
          {error}
        </p>
      ) : null}
    </div>
  );
}
