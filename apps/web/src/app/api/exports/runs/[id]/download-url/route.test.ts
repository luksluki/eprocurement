// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";

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

const download = {
  run_id: "run-1",
  tenant_id: "t-1",
  scheduled_for: "2026-10-28T01:00:00Z",
  status: "success",
  row_count: 7,
  missed_previous: false,
  error_message: "",
  started_at: "2026-10-28T01:00:00Z",
  finished_at: "2026-10-28T01:00:02Z",
  download: {
    url: "http://minio:9000/exports/t-1/2026-10-28/purchase-orders.csv?X-Amz-Signature=sig",
    expires_at: "2026-10-28T10:15:00Z",
  },
};

function request(id = "run-1"): Request {
  return new Request(`http://localhost:3000/api/exports/runs/${id}/download-url`);
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

describe("GET /api/exports/runs/[id]/download-url", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request(), { params: Promise.resolve({ id: "run-1" }) });
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("forwards the run id with the bearer token and passes the envelope through", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(download), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await GET(request("run-42"), { params: Promise.resolve({ id: "run-42" }) });
    expect(res.status).toBe(200);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/exports/runs/run-42/download-url");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });

    const body = (await res.json()) as typeof download;
    expect(body.download.url).toContain("X-Amz-Signature");
  });

  it("passes the 412 export_not_succeeded of a failed run through verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "export run has no file", code: "export_not_succeeded" }), {
            status: 412,
          }),
      ),
    );

    const res = await GET(request(), { params: Promise.resolve({ id: "run-2" }) });
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(412);
    expect(body.code).toBe("export_not_succeeded");
  });

  it("passes the 404 of an unknown or cross-tenant run through verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () => new Response(JSON.stringify({ error: "export run not found", code: "export_run_not_found" }), { status: 404 }),
      ),
    );

    const res = await GET(request("run-other"), { params: Promise.resolve({ id: "run-other" }) });
    const body = (await res.json()) as { code?: string };

    expect(res.status).toBe(404);
    expect(body.code).toBe("export_run_not_found");
  });

  it("surfaces an unreachable gateway as the retryable 502", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new Error("network"))));

    const res = await GET(request(), { params: Promise.resolve({ id: "run-1" }) });

    expect(res.status).toBe(502);
  });
});
