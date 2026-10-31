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
