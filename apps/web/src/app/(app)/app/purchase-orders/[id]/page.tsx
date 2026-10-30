import Link from "next/link";
import { getDictionary, getLocale } from "@/i18n/server";
import { getPurchaseOrder } from "@/lib/gateway";
import { canCreateInvoice } from "@/lib/invoice-eligibility";
import { canAcknowledgePO, canIssuePO, canPostGoodsReceipt } from "@/lib/po-eligibility";
import { requireSession } from "@/lib/session";
import GoodsReceiptForm from "./GoodsReceiptForm";
import POActions from "./POActions";

export const dynamic = "force-dynamic";

// PRD §11: /app/purchase-orders/:id — detail + GRN actions (issue #38).
// Server component: requireSession gates the page, the aggregate comes from
// GET /api/v1/purchase-orders/:id (header + items with their ids in one
// round trip), and the issue/acknowledge buttons and the goods-receipt form
// render only when the pure eligibility helper (lib/po-eligibility,
// mirroring the server's gates) says the viewer may act — the server keeps
// enforcing every gate. A 404 from the gateway renders a not-found state
// instead of crashing (the #34 pattern).
export default async function PurchaseOrderDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const pathname = `/app/purchase-orders/${id}`;
  const session = await requireSession(pathname);
  const [locale, dict] = await Promise.all([getLocale(), getDictionary()]);
  const result = await getPurchaseOrder(session.accessToken, id);

  if (!result.ok) {
    const notFound = result.status === 404;
    return (
      <section aria-labelledby="purchase-order-detail-title">
        <h1 id="purchase-order-detail-title" className="text-2xl font-semibold">
          {dict.purchaseOrders.title}
        </h1>
        <p className="mt-4 text-sm text-red-600" role="alert" data-testid="detail-error">
          {notFound ? dict.purchaseOrderDetail.notFound : dict.purchaseOrderDetail.loadFailed}
        </p>
        <Link href="/app/purchase-orders" className="mt-4 inline-block text-sm font-medium text-slate-700 underline">
          {dict.purchaseOrderDetail.backToList}
        </Link>
      </section>
    );
  }

  const po = result.data;
  const copy = dict.purchaseOrderDetail;
  const localeTag = locale === "id" ? "id-ID" : "en-US";
  const money = (cents: number, currency: string) =>
    new Intl.NumberFormat(localeTag, { style: "currency", currency, maximumFractionDigits: 0 }).format(cents / 100);
  const dateTime = (iso: string) =>
    iso ? new Intl.DateTimeFormat(localeTag, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso)) : "—";

  const statusStyles: Record<string, string> = {
    draft: "bg-slate-100 text-slate-600",
    issued: "bg-amber-50 text-amber-700",
    acknowledged: "bg-emerald-50 text-emerald-700",
    partially_received: "bg-amber-50 text-amber-700",
    received: "bg-emerald-50 text-emerald-700",
    closed: "bg-sky-50 text-sky-700",
    cancelled: "bg-slate-100 text-slate-400",
  };
  const badge = (status: string) => (
    <span
      className={`rounded-full px-2 py-0.5 text-xs font-medium ${statusStyles[status] ?? "bg-slate-100 text-slate-600"}`}
    >
      {status}
    </span>
  );

  // AC "actions hidden when not eligible": the pure helpers decide whether
  // the issue/acknowledge button and the receipt form render; the server
  // re-checks every gate on the call.
  const roles = session.claims.roles;
  const action: "issue" | "acknowledge" | null = canIssuePO(roles, po.status)
    ? "issue"
    : canAcknowledgePO(roles, po.status)
      ? "acknowledge"
      : null;
  const receiptable = canPostGoodsReceipt(roles, po.status);
  // The invoice flow's entry point (#41, maintainer decision 2026-10-29): a
  // Create-invoice link for finance on an invoicable PO (the #39 status
  // set) — the link targets the dedicated /app/invoices/new?po= page.
  const invoiceable = canCreateInvoice(roles, po.status);

  return (
    <section aria-labelledby="purchase-order-detail-title">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <Link href="/app/purchase-orders" className="text-sm font-medium text-slate-500 hover:text-slate-700">
            ← {copy.backToList}
          </Link>
          <h1 id="purchase-order-detail-title" className="mt-1 flex items-center gap-3 text-2xl font-semibold">
            <span data-testid="detail-number">{po.number}</span>
            <span data-testid="detail-status">{badge(po.status)}</span>
          </h1>
        </div>
        <div className="flex flex-col items-end gap-2">
          <div data-testid="po-actions-slot">
            <POActions poId={po.id} kind={action} copy={copy.actions} />
          </div>
          {invoiceable ? (
            <Link
              href={`/app/invoices/new?po=${po.id}`}
              className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50"
              data-testid="po-create-invoice-link"
            >
              {copy.actions.createInvoice}
            </Link>
          ) : null}
        </div>
      </div>

      <dl className="mt-4 grid grid-cols-2 gap-4 rounded-xl border border-slate-200 bg-white p-4 sm:grid-cols-3 lg:grid-cols-5">
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.vendor}</dt>
          <dd className="mt-1 font-mono text-sm" data-testid="detail-vendor">
            {po.vendor_id}
          </dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.currency}</dt>
          <dd className="mt-1 text-sm">{po.currency}</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.total}</dt>
          <dd className="mt-1 text-sm font-semibold" data-testid="detail-total">
            {money(po.total_cents, po.currency)}
          </dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.issued}</dt>
          <dd className="mt-1 text-sm text-slate-500">{dateTime(po.issued_at)}</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-400">{copy.fields.created}</dt>
          <dd className="mt-1 text-sm text-slate-500">{dateTime(po.created_at)}</dd>
        </div>
      </dl>

      <p className="mt-2 text-sm text-slate-500">
        {copy.fields.requisition}:{" "}
        <Link
          href={`/app/requisitions/${po.requisition_id}`}
          className="font-medium text-slate-700 underline"
          data-testid="detail-requisition-link"
        >
          {po.requisition_id}
        </Link>
      </p>

      <h2 className="mt-8 text-lg font-semibold">{copy.items.heading}</h2>
      <table className="mt-2 w-full text-left text-sm" data-testid="detail-items">
        <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
          <tr>
            <th className="py-2 pr-4 font-medium">{copy.items.description}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.items.qtyOrdered}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.items.qtyReceived}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.items.remaining}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.items.unitPrice}</th>
            <th className="py-2 text-right font-medium">{copy.items.lineTotal}</th>
          </tr>
        </thead>
        <tbody>
          {po.items.map((item) => (
            <tr key={item.id} className="border-b border-slate-100 last:border-0">
              <td className="py-2 pr-4">{item.description}</td>
              <td className="py-2 pr-4 text-right">{item.qty_ordered.toLocaleString(localeTag)}</td>
              <td className="py-2 pr-4 text-right">{item.qty_received.toLocaleString(localeTag)}</td>
              <td className="py-2 pr-4 text-right">{(item.qty_ordered - item.qty_received).toLocaleString(localeTag)}</td>
              <td className="py-2 pr-4 text-right">{money(item.unit_price_cents, po.currency)}</td>
              <td className="py-2 text-right font-medium" data-testid="detail-line-total">
                {money(item.qty_ordered * item.unit_price_cents, po.currency)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {receiptable ? (
        <div className="mt-8" data-testid="receipt-form-slot">
          <GoodsReceiptForm poId={po.id} items={po.items} copy={copy.receipt} />
        </div>
      ) : null}
    </section>
  );
}
