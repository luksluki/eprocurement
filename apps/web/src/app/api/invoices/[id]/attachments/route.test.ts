// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

const mocks = vi.hoisted(() => ({
  cookieStore: { get: vi.fn(), set: vi.fn(), delete: vi.fn() },
}));

vi.mock("next/headers", () => ({
  cookies: vi.fn(async () => mocks.cookieStore),
}));

function request(body: unknown): Request {
  return new Request("http://localhost:3000/api/invoices/inv-1/attachments", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

const params = () => ({ params: Promise.resolve({ id: "inv-1" }) });

const declaration = {
  filename: "scan.pdf",
  mime_type: "application/pdf",
  size_bytes: 12,
  checksum_sha256: "ab".repeat(32),
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

describe("POST /api/invoices/[id]/attachments", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(declaration), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects a malformed declaration with 400 body_invalid before any gateway call", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const res = await POST(request({ filename: "scan.pdf" }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(400);
    expect(body.code).toBe("body_invalid");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("forwards the declaration with the bearer token and returns 201 with the upload envelope", async () => {
    const declared = {
      id: "att-1",
      entity_type: "invoice",
      entity_id: "inv-1",
      status: "pending",
      upload: { url: "http://minio:9000/attachments", method: "PUT", expires_at: "2026-10-29T10:15:00Z" },
    };
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify(declared), { status: 201, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request(declaration), params());
    expect(res.status).toBe(201);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/invoices/inv-1/attachments");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
    expect(JSON.parse(init.body as string)).toEqual(declaration);
  });

  it("passes a gateway 404 inv_not_found through (declare checks the invoice first)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invoice not found", code: "inv_not_found" }), { status: 404 })),
    );
    const res = await POST(request(declaration), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(404);
    expect(body.code).toBe("inv_not_found");
  });

  it("passes a gateway 401 through for the refresh dance, and 5xx maps to 502", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invalid or expired token" }), { status: 401 })),
    );
    const res = await POST(request(declaration), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");

    vi.stubGlobal("fetch", vi.fn(async () => new Response("boom", { status: 502 })));
    const res2 = await POST(request(declaration), params());
    expect(res2.status).toBe(502);
  });
});
