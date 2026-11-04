// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below (the events-route pattern).
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
  entries: [
    {
      event_id: "ev-1",
      tenant_id: "t-1",
      actor_id: "u-2",
      actor_ip: "203.0.113.7",
      action: "approve",
      entity_type: "requisition",
      entity_id: "pr-1",
      trace_id: "trace-44",
      occurred_at: "2026-10-30T09:00:00Z",
      recorded_at: "2026-10-30T09:00:01Z",
    },
  ],
  next_cursor: "Y3Vyc29y",
};

function request(query = ""): Request {
  return new Request(`http://localhost:3000/api/audit${query}`);
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

describe("GET /api/audit", () => {
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

  it("forwards the canonical query with the bearer token", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(page), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request("?action=approve&entityType=requisition&entityId=pr-1&limit=20&cursor=abc"));
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(
      "http://gateway.test/api/v1/audit?action=approve&entityType=requisition&entityId=pr-1&limit=20&cursor=abc",
    );
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });

    const body = (await res.json()) as typeof page;
    expect(body.entries[0].actor_ip).toBe("203.0.113.7");
    expect(body.next_cursor).toBe("Y3Vyc29y");
  });

  it("drops garbage params instead of forwarding them", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ entries: [], next_cursor: "" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request("?action=%20approve%20&entityId=&limit=-5&cursor=&other=junk"));
    expect(res.status).toBe(200);
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    // entityId empty, limit non-positive, cursor blank, unknown param — all
    // dropped; action survives trimmed.
    expect(url).toBe("http://gateway.test/api/v1/audit?action=approve");
  });

  it("passes a gateway 400 through with the machine code intact", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              error: "action must be one of approve, reject, issue, post, match",
              code: "audit_action_invalid",
            }),
            { status: 400, headers: { "content-type": "application/json" } },
          ),
      ),
    );

    const res = await GET(request("?action=decide"));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(400);
    expect(body.code).toBe("audit_action_invalid");
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

  it("renders a distinct 403 body for roles without audit:read", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "forbidden", code: "permission_denied", required: "audit:read" }), {
            status: 403,
            headers: { "content-type": "application/json" },
          }),
      ),
    );

    const res = await GET(request());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(403);
    expect(body.code).toBe("permission_denied");
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
