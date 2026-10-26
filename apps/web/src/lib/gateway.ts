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
