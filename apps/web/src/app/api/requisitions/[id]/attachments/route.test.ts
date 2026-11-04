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

const declaration = {
  filename: "quote.pdf",
  mime_type: "application/pdf",
  size_bytes: 12,
  checksum_sha256: "ab".repeat(32),
};

const request = (body: unknown) =>
  new Request("http://localhost:3000/api/requisitions/pr-9/attachments", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/requisitions/[id]/attachments (declare, #33)", () => {
  it("declares through the gateway and returns the presigned envelope", async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse(
        {
          id: "att-1",
          entity_type: "requisition",
          entity_id: "pr-9",
          filename: "quote.pdf",
          status: "pending",
          upload: {
            url: "http://minio:9000/attachments/t/requisition/att-1?X-Amz-Signature=sig",
            method: "PUT",
            expires_at: "2026-10-25T10:15:00Z",
            headers: { "Content-Type": "application/pdf" },
          },
        },
        201,
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request(declaration), params("pr-9"));
    expect(res.status).toBe(201);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/requisitions/pr-9/attachments");
    expect(String(init.body)).toBe(JSON.stringify(declaration));
  });

  it("forwards 403 requisition_forbidden (foreign draft) verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(
          { error: "only the requester (or an admin) may modify this requisition", code: "requisition_forbidden" },
          403,
        ),
      ),
    );
    const res = await POST(request(declaration), params("pr-9"));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(403);
    expect(body.code).toBe("requisition_forbidden");
  });

  it("forwards attachment_too_large (400) and attachment_checksum_mismatch codes verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ error: "file exceeds the size limit", code: "attachment_too_large" }, 400)),
    );
    const res = await POST(request(declaration), params("pr-9"));
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(400);
    expect(body.code).toBe("attachment_too_large");
  });

  it("rejects an incomplete declaration locally", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request({ filename: "quote.pdf" }), params("pr-9"));
    expect(res.status).toBe(400);
    const body = (await res.json()) as { code?: string };
    expect(body.code).toBe("body_invalid");
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
