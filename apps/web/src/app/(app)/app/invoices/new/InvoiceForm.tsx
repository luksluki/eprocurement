"use client";

// Create-invoice form for /app/invoices/new?po=<id> (issue #41, maintainer
// decision 2026-10-29: a dedicated page under /app/invoices, linked from the
// #38 PO detail). One row per PO line — the invoiced quantity plus the unit
// price prefilled with the PO price (#39's contract prices bill against the
// PO line) — and the vendor's invoice number. Client validation mirrors the
// #39 domain rules without a round trip (number non-empty after trimming and
// ≤ 100 chars, qty a whole number > 0, price ≥ 0, at least one filled line);
// the server re-validates everything. Success redirects to the new invoice's
// detail; a 409 inv_over_bill parses the pinned detail line so the exact
// amounts appear inline; 401 runs the refresh dance.
import { useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { callApi } from "@/lib/api-client";
import type { PurchaseOrderItem } from "@/lib/gateway";

// Mirrors the invoice domain bound (invoice.MaxNumberLen).
const MAX_NUMBER_LEN = 100;

export type InvoiceFormCopy = {
  heading: string;
  numberLabel: string;
  numberPlaceholder: string;
  itemsHeading: string;
  itemsHint: string;
  itemsLine: string;
  itemsQty: string;
  itemsUnitPrice: string;
  itemsLineTotal: string;
  submit: string;
  submitting: string;
  numberInvalid: string;
  qtyInvalid: string;
  priceInvalid: string;
  minOneLine: string;
  genericError: string;
  sessionExpired: string;
  errorMessages: Record<string, string>;
  // Template strings with {placeholders} — interpolated with the numbers
  // parsed from the pinned over-bill detail line (amounts formatted as
  // money by the form).
  overBill: string;
};

type LineState = { qty: string; price: string; error: string | null };

// fill interpolates {placeholder} templates (functions cannot cross the
// server→client boundary).
export function fill(template: string, values: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (_, key: string) => (key in values ? String(values[key]) : `{${key}}`));
}

// parseOverBill extracts the pinned amounts from the gateway's over-billing
// error string ("inv_over_bill: po_number=… invoiced=… requested=… total=…"
// — the format pinned in docs/schema/procurement.md). Returns null when the
// shape does not match — the generic message mapping still applies.
export function parseOverBill(
  error: string,
): { poNumber: string; invoiced: number; requested: number; total: number } | null {
  const m = /po_number=(\S+) invoiced=(\d+) requested=(\d+) total=(\d+)/.exec(error);
  if (!m) return null;
  return { poNumber: m[1], invoiced: Number(m[2]), requested: Number(m[3]), total: Number(m[4]) };
}

type CreatedInvoice = { id: string; number: string };

export default function InvoiceForm({
  poId,
  poNumber,
  currency,
  localeTag,
  items,
  copy,
}: {
  poId: string;
  poNumber: string;
  currency: string;
  localeTag: string;
  items: PurchaseOrderItem[];
  copy: InvoiceFormCopy;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const [number, setNumber] = useState("");
  const [numberError, setNumberError] = useState<string | null>(null);
  const [lines, setLines] = useState<Record<string, LineState>>(() =>
    Object.fromEntries(items.map((it) => [it.id, { qty: "", price: String(it.unit_price_cents), error: null }])),
  );
  const [formError, setFormError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  const money = (cents: number) =>
    new Intl.NumberFormat(localeTag, { style: "currency", currency, maximumFractionDigits: 0 }).format(cents / 100);

  // Client-side validation — the #39 domain rules, no round trip. Returns
  // the parsed invoice lines, or null with errors set.
  function validate(): { po_item_id: string; qty: number; unit_price_cents: number }[] | null {
    let ok = true;
    if (number.trim() === "" || number.trim().length > MAX_NUMBER_LEN) {
      setNumberError(copy.numberInvalid);
      ok = false;
    } else {
      setNumberError(null);
    }
    const next = { ...lines };
    const out: { po_item_id: string; qty: number; unit_price_cents: number }[] = [];
    for (const item of items) {
      const line = next[item.id];
      const qty = line.qty.trim();
      const price = line.price.trim();
      if (qty === "") {
        next[item.id] = { ...line, error: null }; // empty = this line is not billed
        continue;
      }
      const qtyNum = Number(qty);
      const priceNum = Number(price);
      if (!Number.isInteger(qtyNum) || qtyNum <= 0) {
        next[item.id] = { ...line, error: copy.qtyInvalid };
        ok = false;
        continue;
      }
      if (price === "" || !Number.isFinite(priceNum) || priceNum < 0) {
        next[item.id] = { ...line, error: copy.priceInvalid };
        ok = false;
        continue;
      }
      next[item.id] = { ...line, error: null };
      // Prices are integer cents (the #39 wire contract).
      out.push({ po_item_id: item.id, qty: qtyNum, unit_price_cents: Math.round(priceNum) });
    }
    setLines(next);
    if (out.length === 0) {
      setFormError(copy.minOneLine);
      return null;
    }
    if (!ok) {
      setFormError(null);
      return null;
    }
    setFormError(null);
    return out;
  }

  async function submit() {
    const parsed = validate();
    if (parsed === null) {
      return; // per-field errors are already on the lines
    }
    setPending(true);
    setFormError(null);
    const result = await callApi<CreatedInvoice>("/api/invoices", {
      method: "POST",
      body: { po_id: poId, number: number.trim(), items: parsed },
    });
    setPending(false);
    if (result.ok) {
      router.push(`/app/invoices/${encodeURIComponent(result.data.id)}`); // the new detail
      return;
    }
    if (result.status === 401) {
      setFormError(copy.sessionExpired);
      window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(pathname)}`);
      return;
    }
    if (result.code === "inv_over_bill") {
      // The pinned detail carries the exact amounts; show them inline.
      const detail = parseOverBill(result.error);
      setFormError(
        detail
          ? fill(copy.overBill, {
              poNumber: detail.poNumber,
              invoiced: money(detail.invoiced),
              requested: money(detail.requested),
              total: money(detail.total),
            })
          : copy.genericError,
      );
      return;
    }
    setFormError(copy.errorMessages[result.code] ?? copy.genericError);
  }

  return (
    <div className="rounded-xl border border-slate-200 bg-white p-4" data-testid="invoice-form">
      <h3 className="text-base font-semibold">
        {copy.heading} — <span className="font-mono text-sm">{poNumber}</span>
      </h3>

      <label className="mt-4 flex flex-col gap-1 text-xs font-medium text-slate-500">
        {copy.numberLabel}
        <input
          type="text"
          value={number}
          onChange={(e) => setNumber(e.target.value)}
          placeholder={copy.numberPlaceholder}
          maxLength={MAX_NUMBER_LEN + 10}
          className="w-72 rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
          data-testid="invoice-number"
        />
      </label>
      {numberError ? (
        <p className="mt-1 text-xs text-red-600" role="alert" data-testid="invoice-number-error">
          {numberError}
        </p>
      ) : null}

      <h4 className="mt-6 text-sm font-semibold">{copy.itemsHeading}</h4>
      <p className="mt-1 text-sm text-slate-500">{copy.itemsHint}</p>
      <table className="mt-2 w-full text-left text-sm" data-testid="invoice-form-lines">
        <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
          <tr>
            <th className="py-2 pr-4 font-medium">{copy.itemsLine}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.itemsQty}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.itemsUnitPrice}</th>
            <th className="py-2 text-right font-medium">{copy.itemsLineTotal}</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => {
            const line = lines[item.id];
            const qtyNum = Number(line.qty);
            const priceNum = Number(line.price);
            const lineTotal =
              line.qty.trim() !== "" && Number.isInteger(qtyNum) && Number.isFinite(priceNum)
                ? money(qtyNum * Math.round(priceNum))
                : "—";
            return (
              <tr key={item.id} className="border-b border-slate-100 last:border-0">
                <td className="py-2 pr-4">
                  {item.description}{" "}
                  <span className="text-xs text-slate-400">
                    ({copy.itemsQty} {item.qty_ordered.toLocaleString(localeTag)})
                  </span>
                </td>
                <td className="py-2 pr-4 text-right">
                  <input
                    type="number"
                    min={1}
                    value={line.qty}
                    onChange={(e) => setLines({ ...lines, [item.id]: { ...line, qty: e.target.value, error: null } })}
                    className="w-24 rounded-lg border border-slate-200 px-2 py-1 text-sm"
                    aria-label={`${copy.itemsQty}: ${item.description}`}
                    data-testid={`invoice-qty-${item.id}`}
                  />
                  {line.error ? (
                    <span className="ml-2 text-xs text-red-600" role="alert" data-testid={`invoice-error-${item.id}`}>
                      {line.error}
                    </span>
                  ) : null}
                </td>
                <td className="py-2 pr-4 text-right">
                  <input
                    type="number"
                    min={0}
                    value={line.price}
                    onChange={(e) => setLines({ ...lines, [item.id]: { ...line, price: e.target.value, error: null } })}
                    className="w-32 rounded-lg border border-slate-200 px-2 py-1 text-sm"
                    aria-label={`${copy.itemsUnitPrice}: ${item.description}`}
                    data-testid={`invoice-price-${item.id}`}
                  />
                </td>
                <td className="py-2 text-right font-medium" data-testid={`invoice-line-total-${item.id}`}>
                  {lineTotal}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>

      <div className="mt-4 flex flex-wrap items-center gap-3">
        <button
          type="button"
          disabled={pending}
          onClick={() => void submit()}
          className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-50"
          data-testid="invoice-submit"
        >
          {copy.submit}
        </button>
        {pending ? (
          <span className="text-xs text-slate-500" data-testid="invoice-pending">
            {copy.submitting}
          </span>
        ) : null}
      </div>

      {formError ? (
        <p className="mt-2 text-sm text-red-600" role="alert" data-testid="invoice-form-error">
          {formError}
        </p>
      ) : null}
    </div>
  );
}
