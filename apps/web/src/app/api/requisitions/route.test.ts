// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below (same pattern as switch-tenant).
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

const page = {
  requisitions: [
    {
      id: "pr-1",
      tenant_id: "t-1",
      number: "PR-000002",
      requester_id: "u-1",
      status: "submitted",
      currency: "IDR",
      total_cents: 15_450_000,
      items: [],
      approval_steps: [],
      submitted_at: "2026-10-24T09:00:00Z",
      created_at: "2026-10-24T08:00:00Z",
      created_by: "u-1",
      updated_at: "2026-10-24T09:00:00Z",
      updated_by: "u-1",
    },
  ],
  total: 42,
  page: 2,
  page_size: 1,
};

function request(query = ""): Request {
  return new Request(`http://localhost:3000/api/requisitions${query}`);
}

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("GET /api/requisitions", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request());
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("canonicalizes filters and forwards them with the bearer token", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(page), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request("?q=pr-0001&status=draft&page=2&page_size=50"));
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/requisitions?q=pr-0001&status=draft&page=2&page_size=50");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });

    const body = (await res.json()) as typeof page;
    expect(body.total).toBe(42);
    expect(body.requisitions[0].number).toBe("PR-000002");
  });

  it("drops garbage inputs instead of forwarding them (clamped to defaults)", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ requisitions: [], total: 0, page: 1, page_size: 20 }), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request("?status=paused&page=-3&page_size=9999&q="));
    expect(res.status).toBe(200);
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    // Unknown status dropped, page clamped to 1, page_size clamped to 100 —
    // and the canonical URL omits the defaults.
    expect(url).toBe("http://gateway.test/api/v1/requisitions?page_size=100");
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

    const res = await GET(request());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
  });

  it("maps any other gateway failure (and network errors) to 502", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "boom" }), {
            status: 500,
            headers: { "content-type": "application/json" },
          }),
      ),
    );
    const res = await GET(request());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(502);
    expect(body.code).toBe("gateway_unavailable");

    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("connection refused");
      }),
    );
    const res2 = await GET(request());
    expect(res2.status).toBe(502);
  });
});
