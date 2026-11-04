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
  return new Request("http://localhost:3000/api/purchase-orders/po-1/goods-receipts", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

const params = () => ({ params: Promise.resolve({ id: "po-1" }) });

beforeEach(() => {
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.get.mockReset();
  mocks.cookieStore.get.mockReturnValue({ value: "at-1" });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("POST /api/purchase-orders/[id]/goods-receipts", () => {
  it("answers 401 without a gateway call when the session cookie is missing", async () => {
    mocks.cookieStore.get.mockReturnValue(undefined);
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request({ items: [{ po_item_id: "it-1", qty_received: 1 }] }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("forwards the trimmed lines and notes with the bearer token; 201 on success", async () => {
    const posted = {
      id: "grn-1",
      number: "GRN-000001",
      po_id: "po-1",
      items: [{ po_item_id: "it-1", qty_received: 2 }],
      purchase_order: { id: "po-1", status: "partially_received", items: [] },
    };
    const fetchMock = vi.fn(
      async () => new Response(JSON.stringify(posted), { status: 201, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request({ items: [{ po_item_id: " it-1 ", qty_received: 2 }], notes: " dock 3 " }), params());
    expect(res.status).toBe(201);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://gateway.test/api/v1/purchase-orders/po-1/goods-receipts");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ authorization: "Bearer at-1" });
    expect(JSON.parse(String(init.body))).toEqual({
      items: [{ po_item_id: "it-1", qty_received: 2 }],
      notes: "dock 3",
    });
    const body = (await res.json()) as { number?: string };
    expect(body.number).toBe("GRN-000001");
  });

  it("validates the shape locally without a gateway call (mirror of the edge)", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    for (const [body, code] of [
      [{}, "grn_items_empty"],
      [{ items: [] }, "grn_items_empty"],
      [{ items: [{ po_item_id: "it-1", qty_received: 0 }] }, "grn_qty_invalid"],
      [{ items: [{ po_item_id: "it-1", qty_received: 1.5 }] }, "grn_qty_invalid"],
      [{ items: [{ po_item_id: "", qty_received: 1 }] }, "grn_qty_invalid"],
      [{ items: [{ po_item_id: "it-1", qty_received: 1 }, { po_item_id: "it-1", qty_received: 1 }] }, "grn_po_item_duplicate"],
      [{ items: [{ po_item_id: "it-1", qty_received: 1 }], notes: "x".repeat(1001) }, "grn_notes_invalid"],
    ] as [unknown, string][]) {
      const res = await POST(request(body), params());
      const parsed = (await res.json()) as { code?: string };
      expect(res.status).toBe(400);
      expect(parsed.code).toBe(code);
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("passes the pinned over-receipt 409 through verbatim (code + full detail)", async () => {
    const detail = "grn_over_receipt: po_item=11111111-0000-0000-0000-000000000001 received=1 requested=2 ordered=2";
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: detail, code: "grn_over_receipt" }), {
            status: 409,
            headers: { "content-type": "application/json" },
          }),
      ),
    );
    const res = await POST(request({ items: [{ po_item_id: "it-1", qty_received: 2 }] }), params());
    const body = (await res.json()) as { code?: string; error?: string };
    expect(res.status).toBe(409);
    expect(body.code).toBe("grn_over_receipt");
    expect(body.error).toBe(detail);
  });

  it("passes a gateway 401 through for the refresh dance", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "invalid or expired token" }), { status: 401 })),
    );
    const res = await POST(request({ items: [{ po_item_id: "it-1", qty_received: 1 }] }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(401);
    expect(body.code).toBe("unauthorized");
  });

  it("maps any other gateway failure (and network errors) to 502", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("boom", { status: 500 })));
    const res = await POST(request({ items: [{ po_item_id: "it-1", qty_received: 1 }] }), params());
    const body = (await res.json()) as { code?: string };
    expect(res.status).toBe(502);
    expect(body.code).toBe("gateway_unavailable");

    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("connection refused");
      }),
    );
    const res2 = await POST(request({ items: [{ po_item_id: "it-1", qty_received: 1 }] }), params());
    expect(res2.status).toBe(502);
  });
});
