// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

// next/headers only works inside a request scope; the route's cookie access
// goes through the mocked store below.
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

const pair = {
  access_token: "at-tenant-b",
  access_expires_in: 900,
  refresh_token: "rt-tenant-b",
  refresh_expires_in: 604800,
  token_type: "Bearer",
};

function request(body: unknown, search = ""): Request {
  return new Request(`http://localhost:3000/api/auth/switch-tenant${search}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

beforeEach(() => {
  process.env.DEMO_TENANTS = "acme=admin@acme.example,glow=admin@glow.example";
  process.env.DEMO_ADMIN_PASSWORD = "";
  process.env.GATEWAY_URL = "http://gateway.test";
  mocks.cookieStore.set.mockClear();
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.DEMO_TENANTS;
  delete process.env.DEMO_ADMIN_PASSWORD;
  delete process.env.GATEWAY_URL;
});

describe("POST /api/auth/switch-tenant", () => {
  it("falls back to a prefilled /login when DEMO_ADMIN_PASSWORD is empty (no gateway call)", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request({ tenant: "glow" }));
    const body = (await res.json()) as { ok: boolean; redirect: string };

    expect(res.status).toBe(200);
    expect(body.ok).toBe(false);
    expect(body.redirect).toBe("/login?email=admin%40glow.example");
    expect(fetchMock).not.toHaveBeenCalled();
    expect(mocks.cookieStore.set).not.toHaveBeenCalled();
  });

  it("rejects unknown tenant slugs with 400", async () => {
    const res = await POST(request({ tenant: "nope" }));
    expect(res.status).toBe(400);
  });

  it("re-logs-in as the target admin and rotates the cookies", async () => {
    process.env.DEMO_ADMIN_PASSWORD = "demo-password";
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(pair), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(request({ tenant: "acme" }, "?next=/app/vendors"));
    const body = (await res.json()) as { ok: boolean; redirect: string };

    expect(res.status).toBe(200);
    expect(body).toEqual({ ok: true, redirect: "/app/vendors" });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      "http://gateway.test/api/v1/auth/login",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ email: "admin@acme.example", password: "demo-password" }),
      }),
    );
    expect(mocks.cookieStore.set).toHaveBeenCalledWith("eproc_at", "at-tenant-b", expect.objectContaining({ httpOnly: true }));
    expect(mocks.cookieStore.set).toHaveBeenCalledWith("eproc_rt", "rt-tenant-b", expect.objectContaining({ httpOnly: true }));
  });

  it("hands over to manual login when the gateway rejects the re-login (401/429)", async () => {
    process.env.DEMO_ADMIN_PASSWORD = "wrong-password";
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "invalid email or password" }), {
            status: 401,
            headers: { "content-type": "application/json" },
          }),
      ),
    );

    const res = await POST(request({ tenant: "acme" }, "?next=/app/vendors"));
    const body = (await res.json()) as { ok: boolean; redirect: string };

    expect(body.ok).toBe(false);
    expect(body.redirect).toBe("/login?email=admin%40acme.example&error=switch_failed");
    expect(mocks.cookieStore.set).not.toHaveBeenCalled();
  });

  it("ignores open-redirect targets in next", async () => {
    process.env.DEMO_ADMIN_PASSWORD = "demo-password";
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify(pair), { status: 200, headers: { "content-type": "application/json" } }),
      ),
    );

    const res = await POST(request({ tenant: "acme" }, "?next=https://evil.example"));
    const body = (await res.json()) as { redirect: string };
    expect(body.redirect).toBe("/app/dashboard"); // safeNext default
  });
});
