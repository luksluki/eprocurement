// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET, PUT } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below (same pattern as the list route).
const mocks = vi.hoisted(() => ({
  cookieStore: {
    get: vi.fn(),
    set: vi.fn(),
    delete: vi.fn(),
  },
}));

vi.mock("next/headers", () => ({
  cookies: vi.fn(async () => mocks.cookieStore),
}));

const detail = {
  id: "pr-9",
  tenant_id: "t-1",
  number: "PR-000009",
  requester_id: "u-1",
  status: "draft",
  currency: "IDR",
  total_cents: 150000,
  items: [{ description: "Laptop", qty: 2, unit_price_cents: 75000, category: "it" }],
  attachments: [
    { id: "att-1", entity_type: "requisition", entity_id: "pr-9", filename: "quote.pdf", status: "ready" },
  ],
  approval_steps: [],
  submitted_at: "",
  created_at: "2026-10-25T08:00:00Z",
  created_by: "u-1",
  updated_at: "2026-10-25T08:00:00Z",
  updated_by: "u-1",
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

const params = (id: string) => ({ params: Promise.resolve({ id }) });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("GET /api/requisitions/[id] (draft restore, #33)", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const res = await GET(new Request("http://localhost:3000/api/requisitions/pr-9"), params("pr-9"));
    expect(res.status).toBe(401);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("returns the detail with items and embedded attachments", async () => {
    const fetchMock = vi.fn(async () => jsonResponse(detail));
    vi.stubGlobal("fetch", fetchMock);
    const res = await GET(new Request("http://localhost:3000/api/requisitions/pr-9"), params("pr-9"));
    expect(res.status).toBe(200);
    const body = (await res.json()) as typeof detail;
    expect(body.items).toHaveLength(1);
    expect(body.attachments?.[0].filename).toBe("quote.pdf");
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/requisitions/pr-9");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
  });

  it("forwards 403 requisition_forbidden verbatim (foreign draft, same tenant)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          { error: "only the requester (or an admin) may modify this requisition", code: "requisition_forbidden" },
          403,
        ),
      ),
    );
    const res = await GET(new Request("http://localhost:3000/api/requisitions/pr-9"), params("pr-9"));
    const body = (await res.json()) as { error?: string; code?: string };
    expect(res.status).toBe(403);
    expect(body.code).toBe("requisition_forbidden");
  });

  it("forwards 404 requisition_not_found verbatim (cross-tenant)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ error: "requisition not found", code: "requisition_not_found" }, 404)),
    );
    const res = await GET(new Request("http://localhost:3000/api/requisitions/pr-9"), params("pr-9"));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(404);
    expect(body.code).toBe("requisition_not_found");
  });

  it("keeps 401 as the refresh-dance signal and collapses 5xx to 502", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "expired" }, 401)));
    const res = await GET(new Request("http://localhost:3000/api/requisitions/pr-9"), params("pr-9"));
    expect(res.status).toBe(401);
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "boom" }, 500)));
    const res2 = await GET(new Request("http://localhost:3000/api/requisitions/pr-9"), params("pr-9"));
    expect(res2.status).toBe(502);
  });
});

describe("PUT /api/requisitions/[id] (basket autosave, #33)", () => {
  const put = (body: unknown) =>
    PUT(
      new Request("http://localhost:3000/api/requisitions/pr-9", {
        method: "PUT",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(body),
      }),
      params("pr-9"),
    );

  it("autosaves the basket with the bearer token", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ ...detail, total_cents: 300000 }));
    vi.stubGlobal("fetch", fetchMock);
    const res = await put({ items: detail.items });
    expect(res.status).toBe(200);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/requisitions/pr-9");
    expect(init.method).toBe("PUT");
    // The update contract carries only items; the empty currency field is
    // the shared body shape the gateway ignores on PUT.
    expect(String(init.body)).toBe(JSON.stringify({ currency: "", items: detail.items }));
  });

  it("forwards 409 requisition_illegal_transition verbatim (non-draft)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({ error: "requisition is no longer a draft", code: "requisition_illegal_transition" }, 409),
      ),
    );
    const res = await put({ items: detail.items });
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(409);
    expect(body.code).toBe("requisition_illegal_transition");
  });

  it("rejects a body without an items array locally", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const res = await put({ currency: "IDR" });
    expect(res.status).toBe(400);
    const body = (await res.json()) as { code?: string };
    expect(body.code).toBe("body_invalid");
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
