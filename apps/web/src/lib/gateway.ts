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
