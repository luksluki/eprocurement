// Pure UI-side gating for the PO issue/acknowledge actions and the
// goods-receipt form (issue #38, AC "actions hidden when not eligible") —
// the approval-eligibility pattern of #34. This mirrors — never replaces —
// the server's rules: the gateway enforces RequirePermission(po:write /
// grn:post) and procurement re-checks the status edges inside the locked
// transactions (services/procurement/purchaseorder). The helper only decides
// whether the controls RENDER; a lost race surfaces as a mapped 4xx
// (po_illegal_transition, grn_over_receipt, …) from the server.

// po:write and grn:post holders per internal/platform/rbac: buyer
// explicitly, admin implicitly (rbac.Check grants admin every permission).
// The two permissions share the same role set today; they stay separate
// functions so a future matrix change never silently widens the other.
const ROLE_ADMIN = "admin";
const ROLE_BUYER = "buyer";

// PO lifecycle statuses (purchaseorder.Status*).
const STATUS_DRAFT = "draft";
const STATUS_ISSUED = "issued";
const STATUS_ACKNOWLEDGED = "acknowledged";
const STATUS_PARTIALLY_RECEIVED = "partially_received";

// holdsPOWrite answers whether the role list grants po:write (buyer, or
// admin holding everything implicitly).
export function holdsPOWrite(roles: string[]): boolean {
  return roles.includes(ROLE_BUYER) || roles.includes(ROLE_ADMIN);
}

// holdsGRNPost answers whether the role list grants grn:post (the same
// buyer/admin pair as po:write).
export function holdsGRNPost(roles: string[]): boolean {
  return roles.includes(ROLE_BUYER) || roles.includes(ROLE_ADMIN);
}

// canIssuePO mirrors the #36 domain gate: only a draft PO issues (anything
// else is 409 po_illegal_transition server-side).
export function canIssuePO(roles: string[], status: string): boolean {
  return holdsPOWrite(roles) && status === STATUS_DRAFT;
}

// canAcknowledgePO mirrors the #36 domain gate: only an issued PO
// acknowledges.
export function canAcknowledgePO(roles: string[], status: string): boolean {
  return holdsPOWrite(roles) && status === STATUS_ISSUED;
}

// canPostGoodsReceipt mirrors the #37 domain gate: goods are received only
// on an acknowledged or partially_received PO (FR-07's partial-receipt
// continuation).
export function canPostGoodsReceipt(roles: string[], status: string): boolean {
  return holdsGRNPost(roles) && (status === STATUS_ACKNOWLEDGED || status === STATUS_PARTIALLY_RECEIVED);
}
