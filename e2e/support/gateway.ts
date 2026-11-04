// Gateway REST helpers for the #59 e2e suite: login, vendor setup/lookup,
// approval-rule install, and the convert-to-PO call the maintainer pinned on
// issue #59 (there is no convert UI — the spec calls
// POST /api/v1/purchase-orders/from-requisition/:id directly with a bearer
// token from the gateway login; the gap is tracked as its own issue).
import { GATEWAY_URL } from "./fixtures";

export class GatewayError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "GatewayError";
  }
}

type LoginPair = {
  access_token: string;
  access_expires_in: number;
  refresh_token: string;
  refresh_expires_in: number;
  token_type: string;
};

// waitForGateway polls GET /healthz until the compose stack answers —
// `docker compose up -d --wait` already gates on health, this keeps the
// setup self-contained when the stack came up a moment ago.
export async function waitForGateway(timeoutMs = 60_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastError = "unknown error";
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${GATEWAY_URL}/healthz`);
      if (res.ok) {
        return;
      }
      lastError = `healthz answered ${res.status}`;
    } catch (err) {
      lastError = err instanceof Error ? err.message : String(err);
    }
    await new Promise((resolve) => setTimeout(resolve, 1_000));
  }
  throw new Error(`gateway at ${GATEWAY_URL} never became healthy: ${lastError}`);
}

// gatewayLogin performs the public login and returns the bearer access token.
export async function gatewayLogin(email: string, password: string): Promise<string> {
  const res = await fetch(`${GATEWAY_URL}/api/v1/auth/login`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ email, password }),
  });
  const body = (await res.json().catch(() => ({}))) as Partial<LoginPair> & { error?: string };
  if (!res.ok || !body.access_token) {
    throw new GatewayError(res.status, "login_failed", `login as ${email} failed (${res.status}): ${body.error ?? "no token"}`);
  }
  return body.access_token;
}

// gatewayFetch is an authenticated JSON call; non-2xx answers throw a
// GatewayError carrying the machine code.
export async function gatewayFetch<T>(
  token: string,
  path: string,
  init: RequestInit = {},
): Promise<{ status: number; data: T }> {
  const res = await fetch(`${GATEWAY_URL}${path}`, {
    ...init,
    headers: {
      authorization: `Bearer ${token}`,
      ...(init.body !== undefined ? { "content-type": "application/json" } : {}),
      ...(init.headers ?? {}),
    },
  });
  const text = await res.text();
  let data: T & { error?: string; code?: string };
  if (!text) {
    data = {} as T & { error?: string; code?: string };
  } else {
    try {
      data = JSON.parse(text) as T & { error?: string; code?: string };
    } catch {
      throw new GatewayError(
        res.status,
        "body_not_json",
        `non-JSON ${init.method ?? "GET"} ${path} answered ${res.status}: ${text.slice(0, 120)}`,
      );
    }
  }
  return { status: res.status, data };
}

export type GatewayVendor = {
  id: string;
  code: string;
  name: string;
  status: string;
};

// ensureVendor creates the tenant's fixture vendor, tolerating the
// vendor_duplicate_code replay of a previous run (onboarding §5.1), then
// returns the vendor id resolved from the list.
export async function ensureVendor(
  token: string,
  vendor: { code: string; name: string; contactEmail: string; taxId: string },
): Promise<string> {
  const created = await gatewayFetch<GatewayVendor & { code?: string }>(token, "/api/v1/vendors", {
    method: "POST",
    body: JSON.stringify({
      code: vendor.code,
      name: vendor.name,
      contact_email: vendor.contactEmail,
      tax_id: vendor.taxId,
    }),
  });
  if (created.status !== 201 && created.status !== 409) {
    throw new GatewayError(created.status, "vendor_create_failed", `creating vendor ${vendor.code} answered ${created.status}`);
  }
  const found = await findVendorByCode(token, vendor.code);
  if (!found) {
    throw new GatewayError(404, "vendor_not_found", `vendor ${vendor.code} missing after create`);
  }
  return found.id;
}

// findVendorByCode resolves a vendor id by exact code through the list API
// (code prefix query keeps it a single small page).
export async function findVendorByCode(token: string, code: string): Promise<GatewayVendor | null> {
  const page = await gatewayFetch<{ vendors: GatewayVendor[] }>(
    token,
    `/api/v1/vendors?q=${encodeURIComponent(code)}&page_size=50`,
  );
  if (page.status !== 200) {
    throw new GatewayError(page.status, "vendor_list_failed", `listing vendors answered ${page.status}`);
  }
  return page.data.vendors.find((v) => v.code === code) ?? null;
}

// installApprovalRules PUTs the single-step IDR chain (onboarding §5.1).
export async function installApprovalRules(token: string, rules: unknown): Promise<void> {
  const res = await gatewayFetch(token, "/api/v1/approval-rules", {
    method: "PUT",
    body: JSON.stringify(rules),
  });
  if (res.status !== 200) {
    throw new GatewayError(res.status, "approval_rules_failed", `PUT approval-rules answered ${res.status}`);
  }
}

export type GatewayPurchaseOrder = {
  id: string;
  number: string;
  status: string;
  vendor_id: string;
};

// convertRequisitionToPO is the maintainer-pinned convert step: a direct
// gateway call (idempotent — 201 created, 200 replay).
export async function convertRequisitionToPO(
  token: string,
  requisitionId: string,
  vendorId: string,
): Promise<GatewayPurchaseOrder> {
  const res = await gatewayFetch<GatewayPurchaseOrder>(
    token,
    `/api/v1/purchase-orders/from-requisition/${encodeURIComponent(requisitionId)}`,
    { method: "POST", body: JSON.stringify({ vendor_id: vendorId }) },
  );
  if (res.status !== 200 && res.status !== 201) {
    throw new GatewayError(res.status, "po_convert_failed", `converting requisition ${requisitionId} answered ${res.status}`);
  }
  return res.data;
}

// ---- #60 resilience spec: the procure-to-pay flow through the gateway ----
// The resilience story is API-driven (the happy-path spec owns the UI
// journey); these helpers are the minimal flow surface the crash/replay
// scenario needs.

export type GatewayApprovalStep = {
  id: string;
  step_no: number;
  status: string;
};

export type GatewayRequisition = {
  id: string;
  number: string;
  status: string;
  approval_steps: GatewayApprovalStep[];
};

export type GatewayPOItem = {
  id: string;
  description: string;
  qty_ordered: number;
  qty_received: number;
  unit_price_cents: number;
};

export type GatewayPurchaseOrderDetail = GatewayPurchaseOrder & {
  items: GatewayPOItem[];
};

// createRequisition POSTs a one-item requisition directly in `submitted`
// terms: it starts as a draft owned by the caller (the API twin of the
// wizard's basket).
export async function createRequisition(
  token: string,
  item: { description: string; qty: number; unitPriceCents: number; category: string },
  currency = "IDR",
): Promise<GatewayRequisition> {
  const res = await gatewayFetch<GatewayRequisition>(token, "/api/v1/requisitions", {
    method: "POST",
    body: JSON.stringify({
      currency,
      items: [
        {
          description: item.description,
          qty: item.qty,
          unit_price_cents: item.unitPriceCents,
          category: item.category,
        },
      ],
    }),
  });
  if (res.status !== 201) {
    throw new GatewayError(res.status, "requisition_create_failed", `creating requisition answered ${res.status}`);
  }
  return res.data;
}

// submitRequisition moves a draft to submitted; the response carries the
// pending approval steps whose ids the decide endpoint needs.
export async function submitRequisition(token: string, requisitionId: string): Promise<GatewayRequisition> {
  const res = await gatewayFetch<GatewayRequisition>(
    token,
    `/api/v1/requisitions/${encodeURIComponent(requisitionId)}/submit`,
    { method: "POST" },
  );
  if (res.status !== 200) {
    throw new GatewayError(res.status, "requisition_submit_failed", `submitting requisition ${requisitionId} answered ${res.status}`);
  }
  return res.data;
}

// decideApproval approves the named step (a reject needs a reason; the
// resilience story only approves). Self-approval is forbidden for everyone
// — the approver token must be a second user of the same tenant.
export async function decideApproval(
  token: string,
  stepId: string,
  decision: "approve" | "reject",
  reason: string,
): Promise<GatewayRequisition> {
  const res = await gatewayFetch<GatewayRequisition>(
    token,
    `/api/v1/approvals/${encodeURIComponent(stepId)}/decide`,
    { method: "POST", body: JSON.stringify({ decision, reason }) },
  );
  if (res.status !== 200) {
    throw new GatewayError(res.status, "approval_decide_failed", `deciding step ${stepId} answered ${res.status}`);
  }
  return res.data;
}

// poAction posts one of the lifecycle actions and returns the updated PO.
async function poAction(
  token: string,
  poId: string,
  action: "issue" | "acknowledge",
): Promise<GatewayPurchaseOrderDetail> {
  const res = await gatewayFetch<GatewayPurchaseOrderDetail>(
    token,
    `/api/v1/purchase-orders/${encodeURIComponent(poId)}/${action}`,
    { method: "POST" },
  );
  if (res.status !== 200) {
    throw new GatewayError(res.status, `po_${action}_failed`, `${action} on PO ${poId} answered ${res.status}`);
  }
  return res.data;
}

export function issuePurchaseOrder(token: string, poId: string): Promise<GatewayPurchaseOrderDetail> {
  return poAction(token, poId, "issue");
}

export function acknowledgePurchaseOrder(token: string, poId: string): Promise<GatewayPurchaseOrderDetail> {
  return poAction(token, poId, "acknowledge");
}

// postGoodsReceipt records one receipt (qty per PO line — a full receipt
// passes each line's ordered qty). The response embeds the updated PO.
export async function postGoodsReceipt(
  token: string,
  poId: string,
  lines: { poItemId: string; qty: number }[],
  notes = "",
): Promise<{ goods_receipt: { id: string; number: string }; purchase_order: GatewayPurchaseOrderDetail }> {
  const res = await gatewayFetch<{
    goods_receipt: { id: string; number: string };
    purchase_order: GatewayPurchaseOrderDetail;
  }>(token, `/api/v1/purchase-orders/${encodeURIComponent(poId)}/goods-receipts`, {
    method: "POST",
    body: JSON.stringify({ items: lines.map((l) => ({ po_item_id: l.poItemId, qty_received: l.qty })), notes }),
  });
  if (res.status !== 201) {
    throw new GatewayError(res.status, "grn_post_failed", `posting goods receipt on ${poId} answered ${res.status}`);
  }
  return res.data;
}

// getPurchaseOrder reads the PO aggregate (header + items with their ids
// and received quantities) — the sync Postgres truth, available whether or
// not the worker is running.
export async function getPurchaseOrder(
  token: string,
  poId: string,
): Promise<GatewayPurchaseOrderDetail> {
  const res = await gatewayFetch<GatewayPurchaseOrderDetail>(
    token,
    `/api/v1/purchase-orders/${encodeURIComponent(poId)}`,
  );
  if (res.status !== 200) {
    throw new GatewayError(res.status, "po_get_failed", `reading PO ${poId} answered ${res.status}`);
  }
  return res.data;
}

export type GatewayEvent = {
  event_id: string;
  type: string;
  entity_type: string;
  entity_id: string;
  occurred_at: string;
};

// listEvents walks GET /api/v1/events through its keyset pages. entityType
// and entityId are optional and must be passed together (documents
// validates both-or-neither); omitting both walks the WHOLE tenant event
// history — the shape the #60 spec uses to group every run-created
// aggregate's events in one pass.
export async function listEvents(
  token: string,
  filter: { entityType?: string; entityId?: string } = {},
): Promise<GatewayEvent[]> {
  const events: GatewayEvent[] = [];
  let cursor = "";
  // A hard stop against a cursor loop; one aggregate carries at most a
  // handful of events, so a few pages are the honest ceiling (a whole
  // tenant needs total/100 pages — still a handful).
  for (let page = 0; page < 100; page++) {
    const query = new URLSearchParams({ limit: "100" });
    if (filter.entityType) {
      query.set("entityType", filter.entityType);
      query.set("entityId", filter.entityId ?? "");
    }
    if (cursor) {
      query.set("cursor", cursor);
    }
    const res = await gatewayFetch<{ events: GatewayEvent[]; next_cursor: string }>(
      token,
      `/api/v1/events?${query.toString()}`,
    );
    if (res.status !== 200) {
      throw new GatewayError(res.status, "events_list_failed", `listing events (${filter.entityType ?? "tenant-wide"}) answered ${res.status}`);
    }
    events.push(...res.data.events);
    cursor = res.data.next_cursor ?? "";
    if (!cursor) {
      return events;
    }
  }
  throw new GatewayError(500, "events_pages_exceeded", `event listing for ${filter.entityType ?? "tenant"} did not terminate`);
}

// listEntityEvents returns every projected event of ONE aggregate — the
// per-entity read the #60 spec's exactly-once assertions group from the
// tenant-wide walk (FR-11's filter shape).
export function listEntityEvents(
  token: string,
  entityType: string,
  entityId: string,
): Promise<GatewayEvent[]> {
  return listEvents(token, { entityType, entityId });
}
