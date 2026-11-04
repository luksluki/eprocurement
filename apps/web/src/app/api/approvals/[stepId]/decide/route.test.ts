// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below (same pattern as the other BFF routes).
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

const decided = {
  id: "pr-1",
  number: "PR-000001",
  status: "approved",
  currency: "IDR",
  total_cents: 1_500_000,
  items: [],
  approval_steps: [],
};

function request(body: unknown, headers?: Record<string, string>): Request {
  return new Request("http://localhost:3000/api/approvals/step-1/decide", {
    method: "POST",
    headers,
    body: JSON.stringify(body),
  });
}

// Route-handler context: params arrives as a promise in Next.js 15.
const params = () => ({ params: Promise.resolve({ stepId: "step-1" }) });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/approvals/[stepId]/decide", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request({ decision: "approve" }), params());
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("forwards the decision, reason, and idempotency key with the bearer token", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(decided), { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(
      request({ decision: "reject", reason: "budget exceeded" }, { "idempotency-key": "key-34" }),
      params(),
    );
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/approvals/step-1/decide");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1", "idempotency-key": "key-34" });
    expect(JSON.parse(String(init.body))).toEqual({ decision: "reject", reason: "budget exceeded" });

    const body = (await res.json()) as typeof decided;
    expect(body.status).toBe("approved");
  });

  it("rejects a non-approve/reject decision locally without a gateway call", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request({ decision: "maybe" }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(400);
    expect(body.code).toBe("approval_decision_invalid");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("passes a gateway 4xx through with the machine code intact", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ error: "this step is not the current pending step", code: "approval_step_not_current" }),
            { status: 409, headers: { "content-type": "application/json" } },
          ),
      ),
    );

    const res = await POST(request({ decision: "approve" }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(409);
    expect(body.code).toBe("approval_step_not_current");
  });

  it("passes a gateway 401 through so the client runs the refresh dance", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "invalid or expired token" }), {
            status: 401,
            headers: { "content-type": "application/json" },
          }),
      ),
    );

    const res = await POST(request({ decision: "approve" }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
  });

  it("maps any other gateway failure (and network errors) to 502", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "boom" }), { status: 500 })),
    );
    const res = await POST(request({ decision: "approve" }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(502);
    expect(body.code).toBe("gateway_unavailable");

    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("connection refused");
      }),
    );
    const res2 = await POST(request({ decision: "approve" }), params());
    expect(res2.status).toBe(502);
  });
});
