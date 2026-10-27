"use client";

// Print button for the requisition detail page (issue #34, "print-friendly
// view"): window.print() against the print styles (sidebar, top bar, actions,
// and the timeline are print:hidden; the document — header, items, steps —
// prints in full). The button hides itself in print.
export default function PrintButton({ label }: { label: string }) {
  return (
    <button
      type="button"
      onClick={() => window.print()}
      className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 print:hidden"
      data-testid="print-button"
    >
      {label}
    </button>
  );
}
