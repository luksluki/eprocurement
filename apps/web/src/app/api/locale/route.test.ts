// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below (same pattern as the switch-tenant
// route test).
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

function request(body: unknown): Request {
  return new Request("http://localhost:3000/api/locale", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

beforeEach(() => {
  mocks.cookieStore.set.mockClear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("POST /api/locale", () => {
  it.each(["id", "en"] as const)("persists locale=%s in the eproc_locale cookie", async (locale) => {
    const res = await POST(request({ locale }));
    const body = (await res.json()) as { ok: boolean; locale: string };

    expect(res.status).toBe(200);
    expect(body).toEqual({ ok: true, locale });
    // Persistence across reloads/navigation: long-lived, path-scoped cookie.
    expect(mocks.cookieStore.set).toHaveBeenCalledWith(
      "eproc_locale",
      locale,
      expect.objectContaining({ path: "/", maxAge: 60 * 60 * 24 * 365, sameSite: "lax" }),
    );
  });

  it("rejects unknown locales with 400 and writes no cookie (failure path)", async () => {
    const res = await POST(request({ locale: "fr" }));
    expect(res.status).toBe(400);
    expect(mocks.cookieStore.set).not.toHaveBeenCalled();
  });

  it("rejects malformed JSON bodies with 400", async () => {
    const res = await POST(
      new Request("http://localhost:3000/api/locale", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: "not-json",
      }),
    );
    expect(res.status).toBe(400);
    expect(mocks.cookieStore.set).not.toHaveBeenCalled();
  });

  it("rejects missing locale with 400", async () => {
    const res = await POST(request({}));
    expect(res.status).toBe(400);
    expect(mocks.cookieStore.set).not.toHaveBeenCalled();
  });
});
