// Gateway access for the BFF: the REST edge per ADR-0001. All functions read
// GATEWAY_URL per call so tests and runtime config can change it.

export function gatewayUrl(): string {
  return process.env.GATEWAY_URL ?? "http://localhost:8080";
}

export type TokenPair = {
  access_token: string;
  access_expires_in: number;
  refresh_token: string;
  refresh_expires_in: number;
  token_type: string;
};

// Claims shape returned by GET /api/v1/me.
export type Claims = {
  userId: string;
  tenantId: string;
  roles: string[];
};

type LoginResult = { ok: true; pair: TokenPair } | { ok: false; status: number };

// gatewayLogin posts credentials to the public gateway login route. Error
// bodies stay generic: identity is the anti-enumeration authority and the
// gateway passes its text through; here only the status matters.
export async function gatewayLogin(email: string, password: string): Promise<LoginResult> {
  return postPair("/api/v1/auth/login", { email, password });
}

async function postPair(path: string, body: unknown): Promise<LoginResult> {
  try {
    const res = await fetch(`${gatewayUrl()}${path}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
    });
    if (!res.ok) {
      return { ok: false, status: res.status };
    }
    return { ok: true, pair: (await res.json()) as TokenPair };
  } catch {
    return { ok: false, status: 502 };
  }
}

// gatewayLogout revokes the refresh token best-effort: the cookie is cleared
// regardless of the outcome, and a failed identity call must not block
// sign-out.
export async function gatewayLogout(refreshToken: string): Promise<void> {
  try {
    await fetch(`${gatewayUrl()}/api/v1/auth/logout`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ refresh_token: refreshToken }),
      cache: "no-store",
    });
  } catch {
    // Network failure: proceed with local sign-out.
  }
}

// fetchMe hydrates claims for server components from the authenticated
// /api/v1/me route. Returns null on any failure (401, 5xx, network) — the
// caller decides between the refresh dance and /login.
export async function fetchMe(accessToken: string): Promise<Claims | null> {
  try {
    const res = await fetch(`${gatewayUrl()}/api/v1/me`, {
      headers: { authorization: `Bearer ${accessToken}` },
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    const body = (await res.json()) as { user_id?: string; tenant_id?: string; roles?: string[] };
    if (!body.user_id || !body.tenant_id || !Array.isArray(body.roles)) {
      return null;
    }
    return { userId: body.user_id, tenantId: body.tenant_id, roles: body.roles };
  } catch {
    return null;
  }
}

export type Vendor = {
  id: string;
  code: string;
  name: string;
  contact_email: string;
  status: string;
};

// Envelope of GET /api/v1/vendors (services/gateway/server/vendors.go):
// { vendors, total, page, page_size } — server-side pagination for the
// DataTable (issue #51).
export type VendorPage = {
  vendors: Vendor[];
  total: number;
  page: number;
  page_size: number;
};

// listVendors loads one tenant-scoped page of the vendor list (issue #27).
// Returns null on failure so the page can render an error state instead of
// crashing.
export async function listVendors(
  accessToken: string,
  params: { page: number; pageSize: number },
): Promise<VendorPage | null> {
  try {
    const url = new URL(`${gatewayUrl()}/api/v1/vendors`);
    url.searchParams.set("page", String(params.page));
    url.searchParams.set("page_size", String(params.pageSize));
    const res = await fetch(url, {
      headers: { authorization: `Bearer ${accessToken}` },
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    const body = (await res.json()) as {
      vendors?: Vendor[];
      total?: number;
      page?: number;
      page_size?: number;
    };
    if (!Array.isArray(body.vendors)) {
      return null;
    }
    return {
      vendors: body.vendors,
      total: body.total ?? body.vendors.length,
      page: body.page ?? params.page,
      page_size: body.page_size ?? params.pageSize,
    };
  } catch {
    return null;
  }
}

// RequisitionHeader is one row of the requisition list (issue #32). List rows
// are header-only: items and approval_steps are always empty on the wire
// (documented in proto/procurement/v1/procurement.proto).
export type RequisitionHeader = {
  id: string;
  tenant_id: string;
  number: string;
  requester_id: string;
  status: string;
  currency: string;
  total_cents: number;
  items: unknown[];
  approval_steps: unknown[];
  submitted_at: string;
  created_at: string;
  created_by: string;
  updated_at: string;
  updated_by: string;
};

export type RequisitionListPage = {
  requisitions: RequisitionHeader[];
  total: number;
  page: number;
  page_size: number;
};

export type RequisitionListResult =
  | { ok: true; data: RequisitionListPage }
  | { ok: false; status: number };

// listRequisitions fetches one page of the tenant-scoped requisition list
// from the gateway. `query` is the already-canonicalized filter string from
// requisition-filters (q/status/page/page_size). Only the status is
// discriminated (401 = session needs the refresh dance; everything else is
// an error state for the UI) so the BFF route stays a thin proxy.
export async function listRequisitions(accessToken: string, query: string): Promise<RequisitionListResult> {
  try {
    const target = `${gatewayUrl()}/api/v1/requisitions${query ? `?${query}` : ""}`;
    const res = await fetch(target, {
      headers: { authorization: `Bearer ${accessToken}` },
      cache: "no-store",
    });
    if (!res.ok) {
      return { ok: false, status: res.status };
    }
    return { ok: true, data: (await res.json()) as RequisitionListPage };
  } catch {
    return { ok: false, status: 502 };
  }
}

// ---------------------------------------------------------------------------
// Requisition form (#33). Unlike listRequisitions above (a thin proxy that
// only discriminates 401), these calls return the gateway's parsed error
// body so the BFF routes can forward 4xx machine codes verbatim — the wizard
// maps them to per-field/per-step messages and stops autosave on 403/409.

// GatewayFailure is a non-OK gateway answer with its machine-readable body
// parsed (`{error, code}` per services/gateway). Network failures surface as
// the synthetic 502/gateway_unavailable pair.
export type GatewayFailure = {
  ok: false;
  status: number;
  error: string;
  code: string;
};

export type GatewayResult<T> = { ok: true; data: T } | GatewayFailure;

async function callGateway<T>(
  accessToken: string,
  path: string,
  init?: { method?: string; body?: unknown; idempotencyKey?: string },
): Promise<GatewayResult<T>> {
  try {
    const headers: Record<string, string> = { authorization: `Bearer ${accessToken}` };
    let body: string | undefined;
    if (init?.body !== undefined) {
      headers["content-type"] = "application/json";
      body = JSON.stringify(init.body);
    }
    if (init?.idempotencyKey) {
      headers["idempotency-key"] = init.idempotencyKey;
    }
    const res = await fetch(`${gatewayUrl()}${path}`, {
      method: init?.method ?? "GET",
      headers,
      body,
      cache: "no-store",
    });
    if (!res.ok) {
      const parsed = (await res.json().catch(() => null)) as { error?: string; code?: string } | null;
      return {
        ok: false,
        status: res.status,
        error: parsed?.error ?? `request failed (${res.status})`,
        code: parsed?.code ?? "gateway_error",
      };
    }
    return { ok: true, data: (await res.json()) as T };
  } catch {
    return { ok: false, status: 502, error: "gateway unreachable", code: "gateway_unavailable" };
  }
}

export type RequisitionItemWire = {
  description: string;
  qty: number;
  unit_price_cents: number;
  category: string;
};

// Attachment metadata as the gateway embeds it (object_key never appears in
// REST responses; clients use the presigned URLs).
export type AttachmentMeta = {
  id: string;
  entity_type: string;
  entity_id: string;
  filename: string;
  mime_type: string;
  size_bytes: number;
  checksum_sha256: string;
  status: string;
  created_by: string;
  created_at: string;
  uploaded_at: string;
};

// ApprovalStep is one row of the requisition detail's approval_steps (the
// gateway's requisitionJSON shape): the sequential workflow with thresholds
// (issue #30). decided_by/decided_at/reason are empty until a decision lands.
export type ApprovalStep = {
  id: string;
  requisition_id: string;
  step_no: number;
  role_required: string;
  threshold_cents: number;
  status: string;
  decided_by: string;
  decided_at: string;
  reason: string;
};

// RequisitionDetail is the GET /api/v1/requisitions/:id shape: the full
// aggregate (typed items and approval steps, unlike the list rows' unknown[])
// plus the embedded attachment metadata.
export type RequisitionDetail = Omit<RequisitionHeader, "items" | "approval_steps"> & {
  items: RequisitionItemWire[];
  approval_steps: ApprovalStep[];
  attachments?: AttachmentMeta[];
};

export type DeclaredUpload = AttachmentMeta & {
  upload: { url: string; method: string; expires_at: string; headers: Record<string, string> };
};

// createRequisitionDraft creates the server-side draft the wizard autosaves
// into. Currency is required (IDR/USD); the basket starts empty and is
// maintained through replaceRequisitionItems.
export function createRequisitionDraft(
  accessToken: string,
  currency: string,
): Promise<GatewayResult<RequisitionDetail>> {
  return callGateway(accessToken, "/api/v1/requisitions", {
    method: "POST",
    body: { currency, items: [] },
  });
}

// replaceRequisitionItems PUTs the whole basket (the update contract carries
// only items — currency is fixed at draft creation).
export function replaceRequisitionItems(
  accessToken: string,
  id: string,
  items: RequisitionItemWire[],
): Promise<GatewayResult<RequisitionDetail>> {
  return callGateway(accessToken, `/api/v1/requisitions/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: { currency: "", items },
  });
}

// getRequisition restores a draft from the server: header, items, and the
// embedded attachment metadata (one round trip).
export function getRequisition(accessToken: string, id: string): Promise<GatewayResult<RequisitionDetail>> {
  return callGateway(accessToken, `/api/v1/requisitions/${encodeURIComponent(id)}`);
}

// submitRequisition moves the draft to submitted. The optional idempotency
// key names the submit; a replay returns the current state.
export function submitRequisition(
  accessToken: string,
  id: string,
  idempotencyKey?: string,
): Promise<GatewayResult<RequisitionDetail>> {
  return callGateway(accessToken, `/api/v1/requisitions/${encodeURIComponent(id)}/submit`, {
    method: "POST",
    idempotencyKey,
  });
}

// decideApprovalStep posts an approve/reject decision for one approval step
// (PRD §15, issue #31). decision ∈ {approve, reject}; a reject requires a
// non-blank reason ≤ 1000 chars — both re-validated at the edge
// (services/gateway/server/approvals.go). The optional idempotency key names
// the decision; a replay on the same step returns the requisition's current
// state. The response is the updated requisition aggregate.
export function decideApprovalStep(
  accessToken: string,
  stepId: string,
  decision: "approve" | "reject",
  reason: string,
  idempotencyKey?: string,
): Promise<GatewayResult<RequisitionDetail>> {
  return callGateway(accessToken, `/api/v1/approvals/${encodeURIComponent(stepId)}/decide`, {
    method: "POST",
    body: { decision, reason },
    idempotencyKey,
  });
}

export type AttachmentDeclaration = {
  filename: string;
  mime_type: string;
  size_bytes: number;
  checksum_sha256: string;
};

// declareAttachment asks the gateway for a presigned PUT URL bound to the
// requisition draft (entity existence + ownership checked gateway-side).
export function declareAttachment(
  accessToken: string,
  requisitionId: string,
  declaration: AttachmentDeclaration,
): Promise<GatewayResult<DeclaredUpload>> {
  return callGateway(accessToken, `/api/v1/requisitions/${encodeURIComponent(requisitionId)}/attachments`, {
    method: "POST",
    body: declaration,
  });
}

// completeAttachment tells the gateway to verify the uploaded object
// (size/mime/checksum) and flip the attachment to ready.
export function completeAttachment(
  accessToken: string,
  requisitionId: string,
  attachmentId: string,
): Promise<GatewayResult<AttachmentMeta>> {
  return callGateway(
    accessToken,
    `/api/v1/requisitions/${encodeURIComponent(requisitionId)}/attachments/${encodeURIComponent(attachmentId)}/complete`,
    { method: "POST" },
  );
}

// EventEntry is one row of the append-only event history (the PRD §14.5
// envelope) as served by GET /api/v1/events (issue #43). payload_json is
// the raw JSON payload string — full fidelity of the procurement outbox
// JSONB, never restructured (dev-note 042).
export type EventEntry = {
  event_id: string;
  tenant_id: string;
  type: string;
  version: number;
  occurred_at: string;
  actor_id: string;
  trace_id: string;
  entity_type: string;
  entity_id: string;
  payload_json: string;
};

// ---------------------------------------------------------------------------
// Purchase orders (#38). The list is a thin proxy (the server component only
// needs data-or-null); the detail and the actions return the typed
// GatewayResult so the BFF routes can forward 4xx machine codes verbatim
// (the wizard/decide convention).

// PurchaseOrderItem is one PO line as the gateway renders it — the item id
// is included because goods-receipt lines reference po_item ids (#37).
export type PurchaseOrderItem = {
  id: string;
  description: string;
  qty_ordered: number;
  qty_received: number;
  unit_price_cents: number;
};

// PurchaseOrderHeader is one row of the PO list (issue #38). List rows are
// header-only: items are always an empty array on the wire (documented in
// proto/procurement/v1/procurement.proto).
export type PurchaseOrderHeader = {
  id: string;
  tenant_id: string;
  number: string;
  requisition_id: string;
  vendor_id: string;
  status: string;
  currency: string;
  total_cents: number;
  items: unknown[];
  issued_at: string;
  created_at: string;
  created_by: string;
  updated_at: string;
  updated_by: string;
};

// Envelope of GET /api/v1/purchase-orders: { purchase_orders, total, page,
// page_size }.
export type PurchaseOrderListPage = {
  purchase_orders: PurchaseOrderHeader[];
  total: number;
  page: number;
  page_size: number;
};

// PurchaseOrderDetail is the GET /api/v1/purchase-orders/:id shape: the full
// aggregate with typed items.
export type PurchaseOrderDetail = Omit<PurchaseOrderHeader, "items"> & {
  items: PurchaseOrderItem[];
};

// listPurchaseOrders loads one tenant-scoped page of the PO list. Returns
// null on any failure so the server-rendered list page can show its error
// state instead of crashing (the listVendors convention).
export async function listPurchaseOrders(
  accessToken: string,
  params: { q: string; status: string; page: number; pageSize: number },
): Promise<PurchaseOrderListPage | null> {
  try {
    const url = new URL(`${gatewayUrl()}/api/v1/purchase-orders`);
    if (params.q !== "") url.searchParams.set("q", params.q);
    if (params.status !== "") url.searchParams.set("status", params.status);
    url.searchParams.set("page", String(params.page));
    url.searchParams.set("page_size", String(params.pageSize));
    const res = await fetch(url, {
      headers: { authorization: `Bearer ${accessToken}` },
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    const body = (await res.json()) as {
      purchase_orders?: PurchaseOrderHeader[];
      total?: number;
      page?: number;
      page_size?: number;
    };
    if (!Array.isArray(body.purchase_orders)) {
      return null;
    }
    return {
      purchase_orders: body.purchase_orders,
      total: body.total ?? body.purchase_orders.length,
      page: body.page ?? params.page,
      page_size: body.page_size ?? params.pageSize,
    };
  } catch {
    return null;
  }
}

// getPurchaseOrder reads the detail aggregate; 404 (also the cross-tenant
// answer) is discriminated so the page can render its not-found state.
export function getPurchaseOrder(accessToken: string, id: string): Promise<GatewayResult<PurchaseOrderDetail>> {
  return callGateway(accessToken, `/api/v1/purchase-orders/${encodeURIComponent(id)}`);
}

// issuePurchaseOrder moves a draft PO to issued (po:write — buyer/admin at
// the gateway; the response is the updated PO).
export function issuePurchaseOrder(accessToken: string, id: string): Promise<GatewayResult<PurchaseOrderDetail>> {
  return callGateway(accessToken, `/api/v1/purchase-orders/${encodeURIComponent(id)}/issue`, { method: "POST" });
}

// acknowledgePurchaseOrder moves an issued PO to acknowledged.
export function acknowledgePurchaseOrder(
  accessToken: string,
  id: string,
): Promise<GatewayResult<PurchaseOrderDetail>> {
  return callGateway(accessToken, `/api/v1/purchase-orders/${encodeURIComponent(id)}/acknowledge`, {
    method: "POST",
  });
}

// GoodsReceiptItemWire is one receipt line of the posting body.
export type GoodsReceiptItemWire = {
  po_item_id: string;
  qty_received: number;
};

// GoodsReceiptPosted is the 201 body of POST goods-receipts: the stored GRN
// with the updated PO embedded.
export type GoodsReceiptPosted = {
  id: string;
  tenant_id: string;
  po_id: string;
  number: string;
  received_by: string;
  received_at: string;
  notes: string;
  items: GoodsReceiptItemWire[];
  created_at: string;
  purchase_order: PurchaseOrderDetail;
};

// postGoodsReceipt records one (partial or full) receipt against a PO
// (grn:post — buyer/admin). A 409 grn_over_receipt carries the pinned detail
// line in `error` ("grn_over_receipt: po_item=… received=… requested=…
// ordered=…"), which the dedicated gateway renderer preserved verbatim.
export function postGoodsReceipt(
  accessToken: string,
  poId: string,
  items: GoodsReceiptItemWire[],
  notes: string,
): Promise<GatewayResult<GoodsReceiptPosted>> {
  return callGateway(accessToken, `/api/v1/purchase-orders/${encodeURIComponent(poId)}/goods-receipts`, {
    method: "POST",
    body: { items, notes },
  });
}


// ---------------------------------------------------------------------------
// Invoices (#41). The list is a thin proxy (the server component only needs
// data-or-null); the detail, create, and match calls return the typed
// GatewayResult so the BFF routes can forward 4xx machine codes verbatim (the
// purchase-order convention of #38).

// InvoiceItemWire is one invoice line as the gateway renders it: the PO line
// it bills, the invoiced quantity, and the invoiced unit price.
export type InvoiceItemWire = {
  po_item_id: string;
  qty: number;
  unit_price_cents: number;
};

// InvoiceHeader is one row of the invoice list (issue #41). List rows are
// header-only: items are always an empty array and match_result_json is
// always "" on the wire (documented in proto/procurement/v1/procurement.proto).
export type InvoiceHeader = {
  id: string;
  tenant_id: string;
  po_id: string;
  vendor_id: string;
  number: string;
  currency: string;
  total_cents: number;
  status: string;
  match_result_json: string;
  items: unknown[];
  created_by: string;
  created_at: string;
};

// Envelope of GET /api/v1/invoices: { invoices, total, page, page_size }.
export type InvoiceListPage = {
  invoices: InvoiceHeader[];
  total: number;
  page: number;
  page_size: number;
};

// InvoiceDetail is the GET /api/v1/invoices/:id shape: the full aggregate
// with typed lines plus the embedded attachment metadata (when documents is
// wired gateway-side).
export type InvoiceDetail = Omit<InvoiceHeader, "items"> & {
  items: InvoiceItemWire[];
  attachments?: AttachmentMeta[];
};

// listInvoices loads one tenant-scoped page of the invoice list. Returns
// null on any failure so the server-rendered list page can show its error
// state instead of crashing (the listPurchaseOrders convention).
export async function listInvoices(
  accessToken: string,
  params: { q: string; status: string; page: number; pageSize: number },
): Promise<InvoiceListPage | null> {
  try {
    const url = new URL(`${gatewayUrl()}/api/v1/invoices`);
    if (params.q !== "") url.searchParams.set("q", params.q);
    if (params.status !== "") url.searchParams.set("status", params.status);
    url.searchParams.set("page", String(params.page));
    url.searchParams.set("page_size", String(params.pageSize));
    const res = await fetch(url, {
      headers: { authorization: `Bearer ${accessToken}` },
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    const body = (await res.json()) as {
      invoices?: InvoiceHeader[];
      total?: number;
      page?: number;
      page_size?: number;
    };
    if (!Array.isArray(body.invoices)) {
      return null;
    }
    return {
      invoices: body.invoices,
      total: body.total ?? body.invoices.length,
      page: body.page ?? params.page,
      page_size: body.page_size ?? params.pageSize,
    };
  } catch {
    return null;
  }
}

// getInvoice reads the detail aggregate (header, lines, match_result_json,
// attachments); 404 (also the cross-tenant answer) is discriminated through
// the shared GatewayResult so the page can render its not-found state.
export function getInvoice(accessToken: string, id: string): Promise<GatewayResult<InvoiceDetail>> {
  return callGateway(accessToken, `/api/v1/invoices/${encodeURIComponent(id)}`);
}

// createInvoice records one vendor invoice against a PO (invoice:write —
// finance/admin; #39 contract). A 409 inv_over_bill carries the pinned detail
// line in `error` ("inv_over_bill: po_number=… invoiced=… requested=…
// total=…"), preserved verbatim by the gateway's dedicated renderer.
export function createInvoice(
  accessToken: string,
  poId: string,
  number: string,
  items: InvoiceItemWire[],
): Promise<GatewayResult<InvoiceDetail>> {
  return callGateway(accessToken, "/api/v1/invoices", {
    method: "POST",
    body: { po_id: poId, number, items },
  });
}

// matchInvoice runs the three-way match on one invoice (invoice:write —
// finance/admin; #40). The response is the updated invoice: status
// matched/discrepancy with match_result_json filled.
export function matchInvoice(accessToken: string, id: string): Promise<GatewayResult<InvoiceDetail>> {
  return callGateway(accessToken, `/api/v1/invoices/${encodeURIComponent(id)}/match`, { method: "POST" });
}

// declareInvoiceAttachment asks the gateway for a presigned PUT URL bound to
// the invoice (entity existence checked gateway-side; invoice:write).
export function declareInvoiceAttachment(
  accessToken: string,
  invoiceId: string,
  declaration: AttachmentDeclaration,
): Promise<GatewayResult<DeclaredUpload>> {
  return callGateway(accessToken, `/api/v1/invoices/${encodeURIComponent(invoiceId)}/attachments`, {
    method: "POST",
    body: declaration,
  });
}

// completeInvoiceAttachment tells the gateway to verify the uploaded object
// (size/mime/checksum) and flip the attachment to ready.
export function completeInvoiceAttachment(
  accessToken: string,
  invoiceId: string,
  attachmentId: string,
): Promise<GatewayResult<AttachmentMeta>> {
  return callGateway(
    accessToken,
    `/api/v1/invoices/${encodeURIComponent(invoiceId)}/attachments/${encodeURIComponent(attachmentId)}/complete`,
    { method: "POST" },
  );
}


// EventsPage is the GET /api/v1/events envelope. Pagination is keyset, not
// offset: next_cursor is the opaque continuation token and is empty on the
// last page — a deliberate deviation from the page/total convention
// documented in dev-notes 042/043.
export type EventsPage = {
  events: EventEntry[];
  next_cursor: string;
};

export type EventsResult =
  | { ok: true; data: EventsPage }
  | { ok: false; status: number; code?: string };

// listEvents fetches one keyset page of the tenant-scoped event history.
// `query` is the already-canonicalized filter string (entityType/entityId/
// limit/cursor — camelCase per PRD §15). 401 is discriminated for the
// refresh dance; 400 carries the gateway's machine code (events_*).
export async function listEvents(accessToken: string, query: string): Promise<EventsResult> {
  try {
    const target = `${gatewayUrl()}/api/v1/events${query ? `?${query}` : ""}`;
    const res = await fetch(target, {
      headers: { authorization: `Bearer ${accessToken}` },
      cache: "no-store",
    });
    if (!res.ok) {
      if (res.status === 400) {
        const body = (await res.json().catch(() => ({}))) as { code?: string };
        return { ok: false, status: 400, code: body.code };
      }
      return { ok: false, status: res.status };
    }
    return { ok: true, data: (await res.json()) as EventsPage };
  } catch {
    return { ok: false, status: 502 };
  }
}

// ExportRunEntry is one nightly-export run record as served by
// GET /api/v1/exports/runs (issue #49, FR-10). status is "success" |
// "failed"; a failed run carries error_message (the basis of #53's error
// display) and no file; missed_previous flags a slot the detection found
// without a successful run.
export type ExportRunEntry = {
  run_id: string;
  tenant_id: string;
  scheduled_for: string;
  status: string;
  row_count: number;
  missed_previous: boolean;
  error_message: string;
  started_at: string;
  finished_at: string;
};

// ExportRunsPage is the GET /api/v1/exports/runs envelope, newest first.
export type ExportRunsPage = {
  runs: ExportRunEntry[];
};

// listExportRuns loads the tenant's export run history (issue #49).
export function listExportRuns(accessToken: string): Promise<GatewayResult<ExportRunsPage>> {
  return callGateway(accessToken, "/api/v1/exports/runs");
}

// ExportDownload is the GET /api/v1/exports/runs/{id}/download-url shape:
// the run's fields plus the download envelope (the attachment convention —
// flat, with the presigned URL nested under "download").
export type ExportDownload = ExportRunEntry & {
  download: { url: string; expires_at: string };
};

// getExportDownloadUrl mints a short-lived presigned GET for one
// successful run's CSV (issue #49). Failed runs answer 412
// export_not_succeeded; another tenant's run id answers 404.
export function getExportDownloadUrl(accessToken: string, runId: string): Promise<GatewayResult<ExportDownload>> {
  return callGateway(accessToken, `/api/v1/exports/runs/${encodeURIComponent(runId)}/download-url`);
}
