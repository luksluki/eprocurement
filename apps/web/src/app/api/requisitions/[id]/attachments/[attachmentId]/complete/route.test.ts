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

const params = (id: string, attachmentId: string) => ({
  params: Promise.resolve({ id, attachmentId }),
});

const request = () =>
  new Request("http://localhost:3000/api/requisitions/pr-9/attachments/att-1/complete", { method: "POST" });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/requisitions/[id]/attachments/[attachmentId]/complete (#33)", () => {
  it("completes through the gateway with the bearer token", async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse({ id: "att-1", entity_id: "pr-9", status: "ready", uploaded_at: "2026-10-25T10:01:00Z" }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request(), params("pr-9", "att-1"));
    expect(res.status).toBe(200);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/requisitions/pr-9/attachments/att-1/complete");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
  });

  it("forwards the verification failures (412 machine codes) verbatim", async () => {
    for (const [code, error] of [
      ["attachment_checksum_mismatch", "uploaded content does not match the declared checksum"],
      ["attachment_object_missing", "uploaded file not found — upload it first"],
    ] as const) {
      vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error, code }, 412)));
      const res = await POST(request(), params("pr-9", "att-1"));
      const body = (await res.json()) as { error?: string; code?: string };
      expect(res.status).toBe(412);
      expect(body.code).toBe(code);
      expect(body.error).toBe(error);
    }
  });

  it("forwards 404 attachment_not_found verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ error: "attachment not found", code: "attachment_not_found" }, 404)),
    );
    const res = await POST(request(), params("pr-9", "att-x"));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(404);
    expect(body.code).toBe("attachment_not_found");
  });

  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request(), params("pr-9", "att-1"));
    expect(res.status).toBe(401);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
