"use client";

// Goods-receipt form for the PO detail page (issue #38, FR-07). One quantity
// input per PO line plus optional notes; validation mirrors the server's
// contract client-side without a round trip (qty a whole number > 0, never
// above the line's remaining qty_ordered − qty_received, at least one filled
// line, notes ≤ 1000 chars). Server failures render inline: the machine code
// maps to a localized message, and the pinned over-receipt detail line
// ("grn_over_receipt: po_item=… received=… requested=… ordered=…", carried
// verbatim by the gateway's dedicated renderer) is parsed so the exact
// numbers appear next to the offending input — the form never crashes. A 401
// runs the refresh dance; a success shows the GRN number and refreshes the
// server-rendered detail.
import { useMemo, useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { callApi } from "@/lib/api-client";
import type { PurchaseOrderItem } from "@/lib/gateway";

// Matches the goodsreceipt domain bound (MaxNotesLen).
const MAX_NOTES_LEN = 1000;

export type ReceiptLineState = { remaining: number; qty: string; error: string | null };

// fill interpolates {placeholder} templates — the dictionary copy crosses
// the server→client boundary as plain strings (functions cannot).
export function fill(template: string, values: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (_, key: string) =>
    key in values ? String(values[key]) : `{${key}}`,
  );
}

export type GoodsReceiptFormCopy = {
  heading: string;
  hint: string;
  qtyLabel: string;
  notesLabel: string;
  notesPlaceholder: string;
  submit: string;
  submitting: string;
  qtyInvalid: string;
  qtyOver: string;
  minOneLine: string;
  notesTooLong: string;
  successTitle: string;
  sessionExpired: string;
  genericError: string;
  errorMessages: Record<string, string>;
  overReceipt: string;
};

type PostedReceipt = { number: string };

// parseOverReceipt extracts the pinned detail numbers from the gateway's
// over-receipt error string (see docs/schema/procurement.md). Returns null
// when the shape does not match — the generic message mapping still applies.
export function parseOverReceipt(error: string): { received: number; requested: number; ordered: number } | null {
  const m = /received=(\d+) requested=(\d+) ordered=(\d+)/.exec(error);
  if (!m) return null;
  return { received: Number(m[1]), requested: Number(m[2]), ordered: Number(m[3]) };
}

// buildLines derives the per-line state from the server items — remaining
// quantities and empty inputs.
function buildLines(items: PurchaseOrderItem[]): Record<string, ReceiptLineState> {
  return Object.fromEntries(
    items.map((it) => [it.id, { remaining: it.qty_ordered - it.qty_received, qty: "", error: null }]),
  );
}

export default function GoodsReceiptForm({
  poId,
  items,
  copy,
}: {
  poId: string;
  items: PurchaseOrderItem[];
  copy: GoodsReceiptFormCopy;
}) {
  const router = useRouter();
  const pathname = usePathname();
  // A successful receipt calls router.refresh(): fresh items arrive as new
  // props. The per-line state must follow them — a stale `lines` snapshot
  // would keep pre-receipt remainders (and leftover quantities) after the
  // refresh, letting a second submit replay an already-received line. The
  // generation key (ids + received quantities) resets the state during
  // render whenever the server data changes (React's adjust-state-when-
  // props-change pattern).
  const generation = useMemo(() => items.map((it) => `${it.id}:${it.qty_received}`).join("|"), [items]);
  const [state, setState] = useState({ generation, lines: buildLines(items) });
  if (state.generation !== generation) {
    setState({ generation, lines: buildLines(items) });
  }
  const lines = state.lines;
  const setLines = (next: Record<string, ReceiptLineState>) => setState({ generation, lines: next });
  const [notes, setNotes] = useState("");
  const [formError, setFormError] = useState<string | null>(null);
  const [grnNumber, setGrnNumber] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  // Client-side per-field validation — no round trip (AC "validation errors
  // inline"). Returns the parsed receipt lines, or null with errors set.
  function validate(): { po_item_id: string; qty_received: number }[] | null {
    const next = { ...lines };
    const out: { po_item_id: string; qty_received: number }[] = [];
    for (const item of items) {
      const line = next[item.id];
      const trimmed = line.qty.trim();
      if (trimmed === "") {
        next[item.id] = { ...line, error: null }; // empty = not received now
        continue;
      }
      const qty = Number(trimmed);
      if (!Number.isInteger(qty) || qty <= 0) {
        next[item.id] = { ...line, error: copy.qtyInvalid };
        continue;
      }
      if (qty > line.remaining) {
        next[item.id] = { ...line, error: fill(copy.qtyOver, { remaining: line.remaining }) };
        continue;
      }
      next[item.id] = { ...line, error: null };
      out.push({ po_item_id: item.id, qty_received: qty });
    }
    setLines(next);
    if (notes.trim().length > MAX_NOTES_LEN) {
      setFormError(copy.notesTooLong);
      return null;
    }
    if (out.length === 0) {
      setFormError(copy.minOneLine);
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
    const result = await callApi<PostedReceipt>(`/api/purchase-orders/${encodeURIComponent(poId)}/goods-receipts`, {
      method: "POST",
      body: { items: parsed, notes: notes.trim() },
    });
    setPending(false);
    if (result.ok) {
      setGrnNumber(result.data.number); // the GRN-%06d confirmation
      router.refresh(); // header/items re-render with the advanced qty
      return;
    }
    if (result.status === 401) {
      setFormError(copy.sessionExpired);
      window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(pathname)}`);
      return;
    }
    if (result.code === "grn_over_receipt") {
      // The pinned detail carries the exact numbers; show them inline on
      // the offending line when it is still on screen, and as the form-level
      // message otherwise (the server is the source of truth — a concurrent
      // receipt may have changed the remainders since the page loaded).
      const detail = parseOverReceipt(result.error);
      setFormError(
        detail
          ? fill(copy.overReceipt, { received: detail.received, requested: detail.requested, ordered: detail.ordered })
          : copy.genericError,
      );
      return;
    }
    setFormError(copy.errorMessages[result.code] ?? copy.genericError);
  }

  return (
    <div className="rounded-xl border border-slate-200 bg-white p-4" data-testid="goods-receipt-form">
      <h3 className="text-base font-semibold">{copy.heading}</h3>
      <p className="mt-1 text-sm text-slate-500">{copy.hint}</p>

      {grnNumber !== null ? (
        <p
          className="mt-3 rounded-lg bg-emerald-50 px-3 py-2 text-sm font-medium text-emerald-700"
          data-testid="grn-confirmation"
          role="status"
        >
          {copy.successTitle}: {grnNumber}
        </p>
      ) : null}

      <table className="mt-3 w-full text-left text-sm">
        <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
          <tr>
            <th className="py-2 pr-4 font-medium">{copy.qtyLabel}</th>
            <th className="py-2 font-medium">→</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => {
            const line = lines[item.id];
            return (
              <tr key={item.id} className="border-b border-slate-100 last:border-0">
                <td className="py-2 pr-4">
                  {item.description}
                  <span className="ml-2 text-xs text-slate-400">({line.remaining})</span>
                </td>
                <td className="py-2">
                  <input
                    type="number"
                    min={1}
                    max={line.remaining}
                    value={line.qty}
                    onChange={(e) => setLines({ ...lines, [item.id]: { ...line, qty: e.target.value, error: null } })}
                    disabled={pending || line.remaining <= 0}
                    className="w-24 rounded-lg border border-slate-200 px-2 py-1 text-sm disabled:bg-slate-50"
                    aria-label={`${copy.qtyLabel}: ${item.description}`}
                    data-testid={`grn-qty-${item.id}`}
                  />
                  {line.error ? (
                    <span className="ml-2 text-xs text-red-600" role="alert" data-testid={`grn-error-${item.id}`}>
                      {line.error}
                    </span>
                  ) : null}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>

      <label className="mt-3 flex flex-col gap-1 text-xs font-medium text-slate-500">
        {copy.notesLabel}
        <textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          placeholder={copy.notesPlaceholder}
          rows={2}
          maxLength={MAX_NOTES_LEN + 1}
          className="w-72 rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
          data-testid="grn-notes"
        />
      </label>

      <div className="mt-3 flex flex-wrap items-center gap-3">
        <button
          type="button"
          disabled={pending}
          onClick={() => void submit()}
          className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-50"
          data-testid="grn-submit"
        >
          {copy.submit}
        </button>
        {pending ? (
          <span className="text-xs text-slate-500" data-testid="grn-pending">
            {copy.submitting}
          </span>
        ) : null}
      </div>

      {formError ? (
        <p className="mt-2 text-sm text-red-600" role="alert" data-testid="grn-form-error">
          {formError}
        </p>
      ) : null}
    </div>
  );
}
