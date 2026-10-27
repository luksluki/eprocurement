import Link from "next/link";
import Timeline from "@/components/Timeline";
import { getDictionary, getLocale } from "@/i18n/server";
import { decidableStep } from "@/lib/approval-eligibility";
import { getRequisition } from "@/lib/gateway";
import { requireSession } from "@/lib/session";
import ApprovalActions from "./ApprovalActions";
import PrintButton from "./PrintButton";

export const dynamic = "force-dynamic";

// PRD §11: /app/requisitions/:id — detail + timeline + actions (issue #34).
// Server component: requireSession gates the page, the aggregate comes from
// GET /api/v1/requisitions/:id (header + items + approval steps + embedded
// attachments in one round trip), and the approve/reject actions render only
// when the pure eligibility helper (lib/approval-eligibility, mirroring the
// server's gates) says the viewer may decide the current step. The timeline
// is the shared #43 component polling /api/events every 2 s (FR-11). Print:
// the chrome (sidebar/top bar live in the shell layout, actions and timeline
// here) is print:hidden, so the printed page is the document itself —
// header, items, approval steps.
export default async function RequisitionDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const pathname = `/app/requisitions/${id}`;
  const session = await requireSession(pathname);
  const [locale, dict] = await Promise.all([getLocale(), getDictionary()]);
  const result = await getRequisition(session.accessToken, id);

  if (!result.ok) {
    const notFound = result.status === 404;
    return (
      <section aria-labelledby="requisition-detail-title">
        <h1 id="requisition-detail-title" className="text-2xl font-semibold">
          {dict.pages.requisitions.title}
        </h1>
        <p className="mt-4 text-sm text-red-600" role="alert" data-testid="detail-error">
          {notFound ? dict.requisitionDetail.notFound : dict.requisitionDetail.loadFailed}
        </p>
        <Link href="/app/requisitions" className="mt-4 inline-block text-sm font-medium text-slate-700 underline">
          {dict.requisitionDetail.backToList}
        </Link>
      </section>
    );
  }

  const requisition = result.data;
  const localeTag = locale === "id" ? "id-ID" : "en-US";
  const money = (cents: number, currency: string) =>
    new Intl.NumberFormat(localeTag, { style: "currency", currency, maximumFractionDigits: 0 }).format(cents / 100);
  const dateTime = (iso: string) =>
    iso ? new Intl.DateTimeFormat(localeTag, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso)) : "—";
  const bytes = (n: number) => new Intl.NumberFormat(localeTag, { style: "unit", unit: "kilobyte" }).format(n / 1024);

  // AC "actions hidden when not permitted": the pure helper decides whether
  // the buttons render (null hides them); the server enforces the same gates
  // on decide.
  const decidable = decidableStep({
    roles: session.claims.roles,
    userId: session.claims.userId,
    requisition: { status: requisition.status, requester_id: requisition.requester_id },
    steps: requisition.approval_steps,
  });

  const statusStyles: Record<string, string> = {
    draft: "bg-slate-100 text-slate-600",
    submitted: "bg-amber-50 text-amber-700",
    approved: "bg-emerald-50 text-emerald-700",
    rejected: "bg-red-50 text-red-700",
    cancelled: "bg-slate-100 text-slate-400",
    closed: "bg-sky-50 text-sky-700",
    pending: "bg-amber-50 text-amber-700",
    skipped: "bg-slate-100 text-slate-400",
  };
  const badge = (status: string) => (
    <span
      className={`rounded-full px-2 py-0.5 text-xs font-medium ${statusStyles[status] ?? "bg-slate-100 text-slate-600"}`}
    >
      {status}
    </span>
  );

  const copy = dict.requisitionDetail;

  return (
    <section aria-labelledby="requisition-detail-title">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <Link href="/app/requisitions" className="text-sm font-medium text-slate-500 hover:text-slate-700">
            ← {copy.backToList}
          </Link>
          <h1 id="requisition-detail-title" className="mt-1 flex items-center gap-3 text-2xl font-semibold">
            <span data-testid="detail-number">{requisition.number}</span>
            <span data-testid="detail-status">{badge(requisition.status)}</span>
          </h1>
        </div>
        <div className="flex flex-col items-end gap-3 print:hidden">
          <PrintButton label={copy.print} />
          <ApprovalActions step={decidable} copy={copy.actions} />
        </div>
      </div>

      <dl className="mt-4 grid grid-cols-2 gap-4 rounded-xl border border-slate-200 bg-white p-4 sm:grid-cols-3 lg:grid-cols-5">
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.requester}</dt>
          <dd className="mt-1 font-mono text-sm" data-testid="detail-requester">
            {requisition.requester_id}
          </dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.currency}</dt>
          <dd className="mt-1 text-sm">{requisition.currency}</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.total}</dt>
          <dd className="mt-1 text-sm font-semibold" data-testid="detail-total">
            {money(requisition.total_cents, requisition.currency)}
          </dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.submitted}</dt>
          <dd className="mt-1 text-sm text-slate-500">{dateTime(requisition.submitted_at)}</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.created}</dt>
          <dd className="mt-1 text-sm text-slate-500">{dateTime(requisition.created_at)}</dd>
        </div>
      </dl>

      <h2 className="mt-8 text-lg font-semibold">{copy.items.heading}</h2>
      {requisition.items.length === 0 ? (
        <p className="mt-2 text-sm text-slate-500">{copy.items.empty}</p>
      ) : (
        <table className="mt-2 w-full text-left text-sm" data-testid="detail-items">
          <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
            <tr>
              <th className="py-2 pr-4 font-medium">{copy.items.description}</th>
              <th className="py-2 pr-4 font-medium">{copy.items.category}</th>
              <th className="py-2 pr-4 text-right font-medium">{copy.items.qty}</th>
              <th className="py-2 pr-4 text-right font-medium">{copy.items.unitPrice}</th>
              <th className="py-2 text-right font-medium">{copy.items.lineTotal}</th>
            </tr>
          </thead>
          <tbody>
            {requisition.items.map((item, i) => (
              <tr key={i} className="border-b border-slate-100 last:border-0">
                <td className="py-2 pr-4">{item.description}</td>
                <td className="py-2 pr-4 text-slate-500">{item.category || "—"}</td>
                <td className="py-2 pr-4 text-right">{item.qty.toLocaleString(localeTag)}</td>
                <td className="py-2 pr-4 text-right">{money(item.unit_price_cents, requisition.currency)}</td>
                <td className="py-2 text-right font-medium" data-testid="detail-line-total">
                  {money(item.qty * item.unit_price_cents, requisition.currency)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2 className="mt-8 text-lg font-semibold">{copy.steps.heading}</h2>
      {requisition.approval_steps.length === 0 ? (
        <p className="mt-2 text-sm text-slate-500">{copy.steps.empty}</p>
      ) : (
        <table className="mt-2 w-full text-left text-sm" data-testid="detail-steps">
          <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
            <tr>
              <th className="py-2 pr-4 font-medium">{copy.steps.step}</th>
              <th className="py-2 pr-4 font-medium">{copy.steps.roleRequired}</th>
              <th className="py-2 pr-4 text-right font-medium">{copy.steps.threshold}</th>
              <th className="py-2 pr-4 font-medium">{copy.steps.status}</th>
              <th className="py-2 pr-4 font-medium">{copy.steps.decidedBy}</th>
              <th className="py-2 pr-4 font-medium">{copy.steps.decidedAt}</th>
              <th className="py-2 font-medium">{copy.steps.reason}</th>
            </tr>
          </thead>
          <tbody>
            {requisition.approval_steps.map((step) => (
              <tr key={step.id} className="border-b border-slate-100 last:border-0">
                <td className="py-2 pr-4 font-medium">{step.step_no}</td>
                <td className="py-2 pr-4">{step.role_required}</td>
                <td className="py-2 pr-4 text-right">{money(step.threshold_cents, requisition.currency)}</td>
                <td className="py-2 pr-4">{badge(step.status)}</td>
                <td className="py-2 pr-4 font-mono text-xs">{step.decided_by || "—"}</td>
                <td className="py-2 pr-4 text-slate-500">{dateTime(step.decided_at)}</td>
                <td className="py-2 text-slate-500">{step.reason || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2 className="mt-8 text-lg font-semibold">{copy.attachments.heading}</h2>
      {!requisition.attachments || requisition.attachments.length === 0 ? (
        <p className="mt-2 text-sm text-slate-500">{copy.attachments.empty}</p>
      ) : (
        <ul className="mt-2 space-y-1 text-sm" data-testid="detail-attachments">
          {requisition.attachments.map((attachment) => (
            <li key={attachment.id} className="flex flex-wrap items-baseline gap-x-3">
              <span className="font-medium">{attachment.filename}</span>
              <span className="text-slate-500">{bytes(attachment.size_bytes)}</span>
              <span>{badge(attachment.status)}</span>
            </li>
          ))}
        </ul>
      )}

      <div className="mt-8 print:hidden">
        <h2 className="text-lg font-semibold">{dict.timeline.title}</h2>
        <Timeline entityType="requisition" entityId={id} locale={locale} copy={dict.timeline} />
      </div>
    </section>
  );
}
