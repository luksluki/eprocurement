// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

const mocks = vi.hoisted(() => ({
  cookieStore: { get: vi.fn(), set: vi.fn(), delete: vi.fn() },
}));

vi.mock("next/headers", () => ({
  cookies: vi.fn(async () => mocks.cookieStore),
}));

function request(): Request {
  return new Request("http://localhost:3000/api/purchase-orders/po-1/issue", { method: "POST" });
}

const params = () => ({ params: Promise.resolve({ id: "po-1" }) });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/purchase-orders/[id]/issue", () => {
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

  it("forwards the path id with the bearer token and returns the updated PO", async () => {
    const issued = { id: "po-1", number: "PO-000001", status: "issued", items: [] };
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify(issued), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(), params());
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/purchase-orders/po-1/issue");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
    expect(init.body).toBeUndefined();
  });

  it("passes a gateway 409 machine code through (po_illegal_transition)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              error: "the purchase order cannot move that way from its current status",
              code: "po_illegal_transition",
            }),
            { status: 409, headers: { "content-type": "application/json" } },
          ),
      ),
    );
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(409);
    expect(body.code).toBe("po_illegal_transition");
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
