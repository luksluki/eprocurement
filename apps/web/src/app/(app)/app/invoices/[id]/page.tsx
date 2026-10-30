import Link from "next/link";
import { Badge } from "@eprocurement/ui";
import { getDictionary, getLocale } from "@/i18n/server";
import { getInvoice } from "@/lib/gateway";
import { canMatchInvoice, hasReadyAttachment, holdsInvoiceWrite } from "@/lib/invoice-eligibility";
import { requireSession } from "@/lib/session";
import InvoiceActions from "./InvoiceActions";
import InvoiceAttachments from "./InvoiceAttachments";
import MatchResultPanel from "./MatchResultPanel";

export const dynamic = "force-dynamic";

// PRD §11: /app/invoices/:id — the detail read that owns the bodies (issue
// #41, the #38 PO-detail pattern): header with the link to the billed PO,
// the lines table (po_item_id, qty, unit price, line total), the attachments
// panel (list + finance upload, the #33 flow), and the MatchResultPanel
// rendering the #40 verdict with the per-line diff — the mismatch cause on
// one screen. The Match button renders only when the pure eligibility helper
// says the viewer may run it, and stays disabled until an attachment is
// ready (the maintainer's hard gate; the server never checks — #40).
export default async function InvoiceDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const pathname = `/app/invoices/${id}`;
  const session = await requireSession(pathname);
  const [locale, dict] = await Promise.all([getLocale(), getDictionary()]);
  const result = await getInvoice(session.accessToken, id);

  if (!result.ok) {
    const notFound = result.status === 404;
    return (
      <section aria-labelledby="invoice-detail-title">
        <h1 id="invoice-detail-title" className="text-2xl font-semibold">
          {dict.invoices.title}
        </h1>
        <p className="mt-4 text-sm text-red-600" role="alert" data-testid="detail-error">
          {notFound ? dict.invoiceDetail.notFound : dict.invoiceDetail.loadFailed}
        </p>
        <Link href="/app/invoices" className="mt-4 inline-block text-sm font-medium text-slate-700 underline">
          {dict.invoiceDetail.backToList}
        </Link>
      </section>
    );
  }

  const inv = result.data;
  const copy = dict.invoiceDetail;
  const localeTag = locale === "id" ? "id-ID" : "en-US";
  const money = (cents: number) =>
    new Intl.NumberFormat(localeTag, { style: "currency", currency: inv.currency, maximumFractionDigits: 0 }).format(
      cents / 100,
    );
  const dateTime = (iso: string) =>
    iso ? new Intl.DateTimeFormat(localeTag, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso)) : "—";

  // Status badge tones (the list's contract): pending neutral, matched
  // success, discrepancy danger.
  const badgeTone = (status: string): "success" | "danger" | "neutral" => {
    if (status === "matched") return "success";
    if (status === "discrepancy") return "danger";
    return "neutral";
  };

  // Eligibility: the pure helper mirrors the #40 re-run guard (server still
  // enforces it); the hard attachment gate disables the button.
  const roles = session.claims.roles;
  const canMatch = canMatchInvoice(roles, inv.status);
  const attachmentReady = hasReadyAttachment(inv.attachments);

  return (
    <section aria-labelledby="invoice-detail-title">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <Link href="/app/invoices" className="text-sm font-medium text-slate-500 hover:text-slate-700">
            ← {copy.backToList}
          </Link>
          <h1 id="invoice-detail-title" className="mt-1 flex items-center gap-3 text-2xl font-semibold">
            <span data-testid="detail-number">{inv.number}</span>
            <span data-testid="detail-status">
              <Badge tone={badgeTone(inv.status)}>{inv.status}</Badge>
            </span>
          </h1>
        </div>
        <div data-testid="invoice-actions-slot">
          <InvoiceActions
            invoiceId={inv.id}
            canMatch={canMatch}
            attachmentReady={attachmentReady}
            copy={copy.actions}
          />
        </div>
      </div>

      <dl className="mt-4 grid grid-cols-2 gap-4 rounded-xl border border-slate-200 bg-white p-4 sm:grid-cols-3 lg:grid-cols-5">
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.vendor}</dt>
          <dd className="mt-1 font-mono text-sm" data-testid="detail-vendor">
            {inv.vendor_id}
          </dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.currency}</dt>
          <dd className="mt-1 text-sm">{inv.currency}</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.total}</dt>
          <dd className="mt-1 text-sm font-semibold" data-testid="detail-total">
            {money(inv.total_cents)}
          </dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.po}</dt>
          <dd className="mt-1 text-sm">
            <Link
              href={`/app/purchase-orders/${inv.po_id}`}
              className="font-medium text-slate-700 underline"
              data-testid="detail-po-link"
            >
              {copy.fields.poLink}
            </Link>
          </dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.created}</dt>
          <dd className="mt-1 text-sm text-slate-500">{dateTime(inv.created_at)}</dd>
        </div>
      </dl>

      <h2 className="mt-8 text-lg font-semibold">{copy.items.heading}</h2>
      <table className="mt-2 w-full text-left text-sm" data-testid="detail-items">
        <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
          <tr>
            <th className="py-2 pr-4 font-medium">{copy.items.poItem}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.items.qty}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.items.unitPrice}</th>
            <th className="py-2 text-right font-medium">{copy.items.lineTotal}</th>
          </tr>
        </thead>
        <tbody>
          {inv.items.map((item) => (
            <tr key={item.po_item_id} className="border-b border-slate-100 last:border-0">
              <td className="py-2 pr-4 font-mono text-xs">{item.po_item_id}</td>
              <td className="py-2 pr-4 text-right">{item.qty.toLocaleString(localeTag)}</td>
              <td className="py-2 pr-4 text-right">{money(item.unit_price_cents)}</td>
              <td className="py-2 text-right font-medium">{money(item.qty * item.unit_price_cents)}</td>
            </tr>
          ))}
        </tbody>
      </table>

      <InvoiceAttachments
        invoiceId={inv.id}
        attachments={inv.attachments ?? []}
        canUpload={holdsInvoiceWrite(roles)}
        copy={copy.attachments}
      />

      <MatchResultPanel matchResultJson={inv.match_result_json} currency={inv.currency} localeTag={localeTag} copy={copy.match} />
    </section>
  );
}
