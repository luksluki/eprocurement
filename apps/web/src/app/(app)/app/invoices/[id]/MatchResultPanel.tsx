import { Badge } from "@eprocurement/ui";
import { fill, parseMatchResult, parseReason } from "@/lib/match-result";

// MatchResultPanel renders the #40 match's stored explanation (issue #41, the
// issue's AC "finance can identify the cause of a mismatch on one screen"):
// the verdict, then one row per invoiced PO line with the quantities the
// engine compared (ordered / received / invoiced + the Δ of invoiced over
// received), the two unit prices (PO vs invoice + Δ in cents), the breach
// badges, and the pinned reason line localized with its parsed numbers —
// empty while the invoice is still pending (never matched).
export type MatchResultPanelCopy = {
  heading: string;
  verdictMatched: string;
  verdictDiscrepancy: string;
  pending: string;
  invalid: string;
  columns: {
    poItem: string;
    qtyOrdered: string;
    qtyReceived: string;
    qtyInvoiced: string;
    qtyDelta: string;
    poPrice: string;
    invoicePrice: string;
    priceDelta: string;
    flags: string;
    reason: string;
  };
  qtyBreach: string;
  priceBreach: string;
  ok: string;
  // Template strings with {placeholders} — interpolated with the numbers
  // parsed from the pinned reason lines.
  reasonQty: string;
  reasonPrice: string;
};

export default function MatchResultPanel({
  matchResultJson,
  currency,
  localeTag,
  copy,
}: {
  matchResultJson: string;
  currency: string;
  localeTag: string;
  copy: MatchResultPanelCopy;
}) {
  const result = parseMatchResult(matchResultJson);
  if (result === null) {
    return (
      <section className="mt-8" data-testid="match-result-pending">
        <h2 className="text-lg font-semibold">{copy.heading}</h2>
        <p className="mt-2 text-sm text-slate-500">{matchResultJson ? copy.invalid : copy.pending}</p>
      </section>
    );
  }

  const money = (cents: number) =>
    new Intl.NumberFormat(localeTag, { style: "currency", currency, maximumFractionDigits: 0 }).format(cents / 100);
  const num = (n: number) => n.toLocaleString(localeTag);
  // Signed delta rendering: +20 / −50,000 (unit: qty / cents respectively).
  const signed = (n: number, asMoney: boolean) =>
    `${n > 0 ? "+" : n < 0 ? "−" : ""}${asMoney ? money(Math.abs(n)) : num(Math.abs(n))}`;

  const reasonText = (reason: string): string => {
    const parsed = parseReason(reason);
    if (parsed === null) return reason; // clean lines never reach here; raw fallback otherwise
    if (parsed.code === "inv_over_bill_qty") {
      return fill(copy.reasonQty, { invoiced: parsed.invoiced, received: parsed.received, pct: parsed.pct });
    }
    return fill(copy.reasonPrice, {
      poPrice: money(parsed.poPrice),
      invoicePrice: money(parsed.invoicePrice),
      pct: parsed.pct,
    });
  };

  return (
    <section className="mt-8" data-testid="match-result-panel">
      <div className="flex items-center gap-3">
        <h2 className="text-lg font-semibold">{copy.heading}</h2>
        <span data-testid="match-verdict">
          <Badge tone={result.matched ? "success" : "danger"}>
            {result.matched ? copy.verdictMatched : copy.verdictDiscrepancy}
          </Badge>
        </span>
      </div>
      <table className="mt-2 w-full text-left text-sm" data-testid="match-lines">
        <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
          <tr>
            <th className="py-2 pr-4 font-medium">{copy.columns.poItem}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.columns.qtyOrdered}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.columns.qtyReceived}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.columns.qtyInvoiced}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.columns.qtyDelta}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.columns.poPrice}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.columns.invoicePrice}</th>
            <th className="py-2 pr-4 text-right font-medium">{copy.columns.priceDelta}</th>
            <th className="py-2 pr-4 font-medium">{copy.columns.flags}</th>
            <th className="py-2 font-medium">{copy.columns.reason}</th>
          </tr>
        </thead>
        <tbody>
          {result.lines.map((line) => (
            <tr key={line.po_item_id} className="border-b border-slate-100 last:border-0">
              <td className="py-2 pr-4 font-mono text-xs">{line.po_item_id}</td>
              <td className="py-2 pr-4 text-right">{num(line.qty_ordered)}</td>
              <td className="py-2 pr-4 text-right">{num(line.qty_received)}</td>
              <td className="py-2 pr-4 text-right">{num(line.qty_invoiced)}</td>
              <td
                className={`py-2 pr-4 text-right ${line.qty_breach ? "font-semibold text-red-600" : ""}`}
                data-testid={`match-qty-delta-${line.po_item_id}`}
              >
                {signed(line.qtyDelta, false)}
              </td>
              <td className="py-2 pr-4 text-right">{money(line.po_unit_price_cents)}</td>
              <td className="py-2 pr-4 text-right">{money(line.invoice_unit_price_cents)}</td>
              <td
                className={`py-2 pr-4 text-right ${line.price_breach ? "font-semibold text-red-600" : ""}`}
                data-testid={`match-price-delta-${line.po_item_id}`}
              >
                {signed(line.priceDeltaCents, true)}
              </td>
              <td className="py-2 pr-4">
                <span className="flex flex-wrap gap-1">
                  {line.qty_breach ? (
                    <span className="rounded-full bg-red-50 px-2 py-0.5 text-xs font-medium text-red-700">
                      {copy.qtyBreach}
                    </span>
                  ) : null}
                  {line.price_breach ? (
                    <span className="rounded-full bg-red-50 px-2 py-0.5 text-xs font-medium text-red-700">
                      {copy.priceBreach}
                    </span>
                  ) : null}
                  {!line.qty_breach && !line.price_breach ? (
                    <span className="rounded-full bg-emerald-50 px-2 py-0.5 text-xs font-medium text-emerald-700">
                      {copy.ok}
                    </span>
                  ) : null}
                </span>
              </td>
              <td className="py-2 text-sm text-slate-600" data-testid={`match-reason-${line.po_item_id}`}>
                {line.reason ? reasonText(line.reason) : "—"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
