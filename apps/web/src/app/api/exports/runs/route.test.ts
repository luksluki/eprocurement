// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below (the events/requisitions pattern).
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
  runs: [
    {
      run_id: "run-1",
      tenant_id: "t-1",
      scheduled_for: "2026-10-28T01:00:00Z",
      status: "success",
      row_count: 7,
      missed_previous: false,
      error_message: "",
      started_at: "2026-10-28T01:00:00Z",
      finished_at: "2026-10-28T01:00:02Z",
    },
    {
      run_id: "run-2",
      tenant_id: "t-1",
      scheduled_for: "2026-10-27T01:00:00Z",
      status: "failed",
      row_count: 0,
      missed_previous: true,
      error_message: "object store unreachable",
      started_at: "2026-10-27T01:00:00Z",
      finished_at: "2026-10-27T01:00:01Z",
    },
  ],
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

describe("GET /api/exports/runs", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET();
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("proxies the run history with the bearer token and passes the body through", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(page), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET();
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/exports/runs");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });

    const body = (await res.json()) as { runs: { run_id: string }[] };
    expect(body.runs.map((r) => r.run_id)).toEqual(["run-1", "run-2"]);
  });

  it("passes a gateway 401 through as 401 (the client runs the refresh dance)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invalid" }), { status: 401 })),
    );

    const res = await GET();
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
  });

  it("passes a gateway 403 through verbatim with its machine code", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ error: "forbidden", code: "permission_denied", required: "export:run" }),
            { status: 403 },
          ),
      ),
    );

    const res = await GET();
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(403);
    // The BFF forwards only {error, code} — the machine code is what the
    // client maps on (the "required" field stays at the gateway).
    expect(body.code).toBe("permission_denied");
  });

  it("surfaces an unreachable gateway as the retryable 502", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new Error("network"))));

    const res = await GET();
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(502);
    expect(body.code).toBe("gateway_unavailable");
  });
});
