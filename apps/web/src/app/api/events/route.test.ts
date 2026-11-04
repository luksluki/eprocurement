// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { eventsToSearchParams } from "@/lib/events-query";
import { GET } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below (same pattern as requisitions).
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
  events: [
    {
      event_id: "ev-1",
      tenant_id: "t-1",
      type: "pr.approved",
      version: 1,
      occurred_at: "2026-10-24T09:00:00Z",
      actor_id: "u-2",
      trace_id: "trace-1",
      entity_type: "requisition",
      entity_id: "pr-1",
      payload_json: '{"status":"approved"}',
    },
  ],
  next_cursor: "Y3Vyc29y",
};

function request(query = ""): Request {
  return new Request(`http://localhost:3000/api/events${query}`);
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

describe("GET /api/events", () => {
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

  it("canonicalizes the query and forwards it with the bearer token", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(page), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request("?entityType=requisition&entityId=pr-1&limit=20&cursor=abc"));
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/events?entityType=requisition&entityId=pr-1&cursor=abc&limit=20");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });

    const body = (await res.json()) as typeof page;
    expect(body.events[0].event_id).toBe("ev-1");
    expect(body.next_cursor).toBe("Y3Vyc29y");
  });

  it("drops garbage params instead of forwarding them", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ events: [], next_cursor: "" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request("?entityType=%20requisition%20&entityId=&limit=-5&cursor=&other=junk"));
    expect(res.status).toBe(200);
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    // entityId empty, limit non-positive, cursor blank, unknown param — all
    // dropped; entityType survives trimmed.
    expect(url).toBe("http://gateway.test/api/v1/events?entityType=requisition");
  });

  it("passes a gateway 400 through with the machine code intact", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              error: "entityType and entityId must both be set or both empty",
              code: "events_filter_invalid",
            }),
            { status: 400, headers: { "content-type": "application/json" } },
          ),
      ),
    );

    const res = await GET(request("?entityType=requisition"));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(400);
    expect(body.code).toBe("events_filter_invalid");
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

describe("eventsToSearchParams", () => {
  it("drops a non-numeric limit and keeps a positive integer", () => {
    expect(eventsToSearchParams(new URLSearchParams("limit=abc")).toString()).toBe("");
    expect(eventsToSearchParams(new URLSearchParams("limit=20")).toString()).toBe("limit=20");
  });
});
