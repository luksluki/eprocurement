// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

const mocks = vi.hoisted(() => ({
  cookieStore: { get: vi.fn(), set: vi.fn(), delete: vi.fn() },
}));

vi.mock("next/headers", () => ({
  cookies: vi.fn(async () => mocks.cookieStore),
}));

const VENDOR_ID = "11111111-2222-4333-8444-555555555555";

function request(body?: string): Request {
  return new Request("http://localhost:3000/api/purchase-orders/from-requisition/pr-1", {
    method: "POST",
    headers: { "x-forwarded-for": "203.0.113.7" },
    body: body ?? JSON.stringify({ vendor_id: VENDOR_ID }),
  });
}

const params = () => ({ params: Promise.resolve({ id: "pr-1" }) });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/purchase-orders/from-requisition/[id]", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("mirrors the edge locally: malformed JSON answers 400 body_invalid without a gateway call", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request("{not json"), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(400);
    expect(body.code).toBe("body_invalid");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("mirrors the edge locally: a non-UUID/empty vendor_id answers 400 po_vendor_id_invalid without a gateway call", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    for (const bad of [JSON.stringify({ vendor_id: "acme-01" }), JSON.stringify({ vendor_id: "  " }), JSON.stringify({})]) {
      const res = await POST(request(bad), params());
      const body = (await res.json()) as { code?: string };
      expect(res.status).toBe(400);
      expect(body.code).toBe("po_vendor_id_invalid");
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("forwards the path id, vendor_id, bearer, and X-Forwarded-For; returns 201 with the draft PO", async () => {
    const draft = { id: "po-1", number: "PO-000001", status: "draft", items: [] };
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify(draft), { status: 201, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(), params());
    expect(res.status).toBe(201);
    expect(await res.json()).toEqual(draft);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/purchase-orders/from-requisition/pr-1");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({
      authorization: "Bearer at-1",
      "x-forwarded-for": "203.0.113.7",
    });
    expect(JSON.parse(String(init.body))).toEqual({ vendor_id: VENDOR_ID });
  });

  it("keeps a gateway 200 replay as 200 (already converted is not an error)", async () => {
    const existing = { id: "po-1", number: "PO-000001", status: "draft", items: [] };
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify(existing), { status: 200, headers: { "content-type": "application/json" } })),
    );

    const res = await POST(request(), params());
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual(existing);
  });

  it.each([
    { status: 409, code: "po_requisition_not_approved" },
    { status: 412, code: "vendor_deactivated" },
    { status: 404, code: "vendor_not_found" },
    { status: 404, code: "requisition_not_found" },
    { status: 403, code: "permission_denied" },
  ])("passes a gateway $status machine code through ($code)", async ({ status, code }) => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "gate failed", code }), {
            status,
            headers: { "content-type": "application/json" },
          }),
      ),
    );
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(status);
    expect(body.code).toBe(code);
  });

  it("passes a gateway 401 through for the refresh dance", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invalid or expired token" }), { status: 401 })),
    );
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
  });

  it("maps any other gateway failure (and network errors) to 502", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("boom", { status: 500 })));
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(502);
    expect(body.code).toBe("gateway_unavailable");

    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("connection refused");
      }),
    );
    const res2 = await POST(request(), params());
    expect(res2.status).toBe(502);
  });
});
