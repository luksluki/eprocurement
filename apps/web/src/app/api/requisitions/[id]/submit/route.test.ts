// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

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

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

const params = (id: string) => ({ params: Promise.resolve({ id }) });

const request = (headers: Record<string, string> = {}) =>
  new Request("http://localhost:3000/api/requisitions/pr-9/submit", { method: "POST", headers });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/requisitions/[id]/submit (#33)", () => {
  it("submits through the gateway with the idempotency key header", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ id: "pr-9", status: "submitted", number: "PR-000009" }));
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request({ "idempotency-key": "key-33-a" }), params("pr-9"));
    expect(res.status).toBe(200);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/requisitions/pr-9/submit");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1", "idempotency-key": "key-33-a" });
  });

  it("forwards 412 approval_rules_missing verbatim for per-step mapping", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          { error: "no approval rules match this requisition", code: "approval_rules_missing" },
          412,
        ),
      ),
    );
    const res = await POST(request(), params("pr-9"));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(412);
    expect(body.code).toBe("approval_rules_missing");
  });

  it("forwards 409 requisition_illegal_transition and 400 requisition_items_empty verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({ error: "requisition is no longer a draft", code: "requisition_illegal_transition" }, 409),
      ),
    );
    const res = await POST(request(), params("pr-9"));
    expect(res.status).toBe(409);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({ error: "add at least one item before submitting", code: "requisition_items_empty" }, 400),
      ),
    );
    const res2 = await POST(request(), params("pr-9"));
    const body2 = (await res2.json()) as { code?: string };
    expect(res2.status).toBe(400);
    expect(body2.code).toBe("requisition_items_empty");
  });

  it("rejects an over-long idempotency key locally", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request({ "idempotency-key": "k".repeat(65) }), params("pr-9"));
    expect(res.status).toBe(400);
    const body = (await res.json()) as { code?: string };
    expect(body.code).toBe("requisition_idempotency_key_invalid");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request(), params("pr-9"));
    expect(res.status).toBe(401);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
