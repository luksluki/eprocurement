// Pure UI-side gating for the invoice actions (issue #41, AC "Match button
// disabled until a ready attachment exists" + "Create invoice link for
// finance on invoicable POs") — the po-eligibility pattern of #38. This
// mirrors — never replaces — the server's rules: the gateway enforces
// RequirePermission(invoice:write) and procurement re-checks the PO status
// gate and the matched re-run guard inside the locked transactions. The
// helper only decides whether the controls RENDER and whether the Match
// button is clickable; a lost race surfaces as a mapped 4xx
// (inv_already_matched, po_illegal_transition, …) from the server.
//
// The attachment gate is deliberately client-side only (maintainer decision
// 2026-10-29): #40 keeps the server non-checking, so the UI carries the demo
// flow's "attachment required" obligation as a hard disable.

import type { AttachmentMeta } from "@/lib/gateway";

// invoice:write holders per internal/platform/rbac: finance explicitly,
// admin implicitly (rbac.Check grants admin every permission).
const ROLE_ADMIN = "admin";
const ROLE_FINANCE = "finance";

// Invoice statuses (invoice.Status*) and the #40 re-match guard: matched is
// final; pending and discrepancy may be (re-)matched.
const STATUS_PENDING = "pending";
const STATUS_DISCREPANCY = "discrepancy";

// The invoicable PO statuses (the #39/#37 GRN set): only
// acknowledged/partially_received/received POs may be billed.
const PO_ACKNOWLEDGED = "acknowledged";
const PO_PARTIALLY_RECEIVED = "partially_received";
const PO_RECEIVED = "received";

// holdsInvoiceWrite answers whether the role list grants invoice:write
// (finance, or admin holding everything implicitly).
export function holdsInvoiceWrite(roles: string[]): boolean {
  return roles.includes(ROLE_FINANCE) || roles.includes(ROLE_ADMIN);
}

// canMatchInvoice mirrors the #40 re-run guard: a finance/admin viewer may
// run the match while the invoice is pending or discrepancy (matched is
// final — the server answers 409 inv_already_matched).
export function canMatchInvoice(roles: string[], invoiceStatus: string): boolean {
  return (
    holdsInvoiceWrite(roles) && (invoiceStatus === STATUS_PENDING || invoiceStatus === STATUS_DISCREPANCY)
  );
}

// canCreateInvoice mirrors the #39 PO-status gate: finance/admin may bill an
// acknowledged, partially_received, or received PO.
export function canCreateInvoice(roles: string[], poStatus: string): boolean {
  return (
    holdsInvoiceWrite(roles) &&
    (poStatus === PO_ACKNOWLEDGED || poStatus === PO_PARTIALLY_RECEIVED || poStatus === PO_RECEIVED)
  );
}

// hasReadyAttachment answers whether any embedded attachment is ready — the
// hard gate's condition (maintainer decision 2026-10-29: the Match button
// stays disabled until a vendor attachment reached ready, so the match never
// runs without the supporting document; uploads sit at pending until
// complete verifies them).
export function hasReadyAttachment(attachments: AttachmentMeta[] | undefined): boolean {
  return (attachments ?? []).some((a) => a.status === "ready");
}
