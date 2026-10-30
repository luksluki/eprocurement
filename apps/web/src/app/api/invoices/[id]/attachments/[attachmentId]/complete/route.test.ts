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
  return new Request("http://localhost:3000/api/invoices/inv-1/attachments/att-1/complete", { method: "POST" });
}

const params = () => ({ params: Promise.resolve({ id: "inv-1", attachmentId: "att-1" }) });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/invoices/[id]/attachments/[attachmentId]/complete", () => {
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

  it("forwards both path ids with the bearer token and returns the ready attachment", async () => {
    const ready = { id: "att-1", entity_type: "invoice", entity_id: "inv-1", status: "ready" };
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify(ready), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(), params());
    expect(res.status).toBe(200);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/invoices/inv-1/attachments/att-1/complete");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
  });

  it("passes a gateway 412 attachment_object_missing through", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "upload did not reach storage", code: "attachment_object_missing" }), {
            status: 412,
          }),
      ),
    );
    const res = await POST(request(), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(412);
    expect(body.code).toBe("attachment_object_missing");
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
