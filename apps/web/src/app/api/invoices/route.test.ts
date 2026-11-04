// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

const mocks = vi.hoisted(() => ({
  cookieStore: { get: vi.fn(), set: vi.fn(), delete: vi.fn() },
}));

vi.mock("next/headers", () => ({
  cookies: vi.fn(async () => mocks.cookieStore),
}));

function request(body: unknown): Request {
  return new Request("http://localhost:3000/api/invoices", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

const validBody = {
  po_id: "po-1",
  number: "INV/2026/0001",
  items: [{ po_item_id: "po-item-1", qty: 2, unit_price_cents: 7500000 }],
};

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/invoices", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(validBody));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects malformed bodies with 400 body_invalid before any gateway call", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    for (const bad of [
      null,
      { po_id: "po-1", number: "INV-1", items: [] },
      { po_id: "po-1", number: "INV-1", items: [{ po_item_id: "a", qty: 0, unit_price_cents: 1 }] },
      { po_id: "po-1", number: "INV-1", items: [{ po_item_id: "a", qty: 1.5, unit_price_cents: 1 }] },
      { po_id: "po-1", number: "INV-1", items: [{ po_item_id: "a", qty: 1, unit_price_cents: -1 }] },
      { po_id: "po-1", items: [{ po_item_id: "a", qty: 1, unit_price_cents: 1 }] },
    ]) {
      const res = await POST(request(bad));
      const body = (await res.json()) as { code?: string };
      expect(res.status).toBe(400);
      expect(body.code).toBe("body_invalid");
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("forwards the body with the bearer token and returns 201", async () => {
    const created = { id: "inv-1", number: "INV/2026/0001", status: "pending", items: [] };
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify(created), { status: 201, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(validBody));
    expect(res.status).toBe(201);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/invoices");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
    expect(JSON.parse(init.body as string)).toEqual(validBody);
  });

  it("passes the pinned 409 inv_over_bill detail line through verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              error: "inv_over_bill: po_number=PO-000001 invoiced=6000000 requested=6000000 total=10000000",
              code: "inv_over_bill",
            }),
            { status: 409, headers: { "content-type": "application/json" } },
          ),
      ),
    );
    const res = await POST(request(validBody));
    const body = (await res.json()) as { code?: string; error?: string };
    expect(res.status).toBe(409);
    expect(body.code).toBe("inv_over_bill");
    expect(body.error).toBe("inv_over_bill: po_number=PO-000001 invoiced=6000000 requested=6000000 total=10000000");
  });

  it("passes other gateway 4xx codes through (inv_number_duplicate)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "this vendor already has an invoice with this number", code: "inv_number_duplicate" }), {
            status: 409,
            headers: { "content-type": "application/json" },
          }),
      ),
    );
    const res = await POST(request(validBody));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(409);
    expect(body.code).toBe("inv_number_duplicate");
  });

  it("passes a gateway 401 through for the refresh dance", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invalid or expired token" }), { status: 401 })),
    );
    const res = await POST(request(validBody));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
  });

  it("maps any other gateway failure (and network errors) to 502", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("boom", { status: 500 })));
    const res = await POST(request(validBody));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(502);
    expect(body.code).toBe("gateway_unavailable");

    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("connection refused");
      }),
    );
    const res2 = await POST(request(validBody));
    expect(res2.status).toBe(502);
  });
});
