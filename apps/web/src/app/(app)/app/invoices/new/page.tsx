import Link from "next/link";
import { getDictionary, getLocale } from "@/i18n/server";
import { getPurchaseOrder } from "@/lib/gateway";
import { canCreateInvoice } from "@/lib/invoice-eligibility";
import { requireSession } from "@/lib/session";
import InvoiceForm from "./InvoiceForm";

export const dynamic = "force-dynamic";

// PRD §11 / maintainer decision 2026-10-29: /app/invoices/new?po=<id> — the
// create-invoice page, linked from the #38 PO detail for finance on an
// invoicable PO (acknowledged/partially_received/received). The server
// component loads the PO once (its items carry the ids the invoice lines
// reference and the prices the form prefills), gates the page on the pure
// eligibility helper, and renders the client form; the server re-checks
// every gate on the POST (#39's guards).
export default async function NewInvoicePage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const params = await searchParams;
  const poParam = params.po;
  const poId = ((Array.isArray(poParam) ? poParam[0] : poParam) ?? "").trim();
  const pathname = poId ? `/app/invoices/new?po=${encodeURIComponent(poId)}` : "/app/invoices/new";
  const session = await requireSession(pathname);
  const [locale, dict] = await Promise.all([getLocale(), getDictionary()]);
  const copy = dict.invoiceForm;
  const localeTag = locale === "id" ? "id-ID" : "en-US";

  // Resolve the PO and the viewer's eligibility before rendering: a missing
  // po param, an unknown PO, and a viewer without invoice:write on an
  // invoicable PO each render their own state instead of the form.
  let body: React.ReactNode;
  if (poId === "") {
    body = (
      <p className="mt-4 text-sm text-red-600" role="alert" data-testid="new-invoice-error">
        {copy.missingPO}
      </p>
    );
  } else {
    const result = await getPurchaseOrder(session.accessToken, poId);
    if (!result.ok) {
      body = (
        <p className="mt-4 text-sm text-red-600" role="alert" data-testid="new-invoice-error">
          {result.status === 404 ? copy.poNotFound : copy.loadFailed}
        </p>
      );
    } else if (!canCreateInvoice(session.claims.roles, result.data.status)) {
      body = (
        <p className="mt-4 text-sm text-amber-700" role="alert" data-testid="new-invoice-not-allowed">
          {copy.notAllowed}
        </p>
      );
    } else {
      const po = result.data;
      body = (
        <div className="mt-4">
          <InvoiceForm
            poId={po.id}
            poNumber={po.number}
            currency={po.currency}
            localeTag={localeTag}
            items={po.items}
            copy={copy.form}
          />
        </div>
      );
    }
  }

  return (
    <section aria-labelledby="new-invoice-title">
      <Link
        href={poId ? `/app/purchase-orders/${poId}` : "/app/invoices"}
        className="text-sm font-medium text-slate-500 hover:text-slate-700"
      >
        ← {copy.back}
      </Link>
      <h1 id="new-invoice-title" className="mt-1 text-2xl font-semibold">
        {copy.title}
      </h1>
      {body}
    </section>
  );
}
