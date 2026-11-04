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
  return new Request("http://localhost:3000/api/invoices/inv-1/match", { method: "POST" });
}

const params = () => ({ params: Promise.resolve({ id: "inv-1" }) });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/invoices/[id]/match", () => {
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

  it("forwards the path id with the bearer token and returns the updated invoice", async () => {
    const matched = { id: "inv-1", status: "matched", match_result_json: '{"matched":true,"lines":[]}' };
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify(matched), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(), params());
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/invoices/inv-1/match");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
    expect(init.body).toBeUndefined();
  });

  it("passes a gateway 409 inv_already_matched through", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ error: "this invoice is already matched; matching is final", code: "inv_already_matched" }),
            { status: 409, headers: { "content-type": "application/json" } },
          ),
      ),
    );
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(409);
    expect(body.code).toBe("inv_already_matched");
  });

  it("passes a gateway 404 inv_not_found through", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invoice not found", code: "inv_not_found" }), { status: 404 })),
    );
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(404);
    expect(body.code).toBe("inv_not_found");
  });

  it("passes a gateway 401 through for the refresh dance, and 5xx maps to 502", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invalid or expired token" }), { status: 401 })),
    );
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");

    vi.stubGlobal("fetch", vi.fn(async () => new Response("boom", { status: 500 })));
    const res2 = await POST(request(), params());
    expect(res2.status).toBe(502);
  });
});
