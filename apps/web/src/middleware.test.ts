// @vitest-environment node
import { describe, expect, it } from "vitest";
import { NextRequest } from "next/server";
import { middleware, config } from "@/middleware";
import { ACCESS_COOKIE, REFRESH_COOKIE } from "@/lib/constants";

function request(path: string, cookie?: string): NextRequest {
  return new NextRequest(`http://localhost${path}`, {
    headers: cookie ? { cookie } : {},
  });
}

describe("middleware session presence check", () => {
  it("redirects unauthenticated /app routes to /login with the next parameter (307)", () => {
    const res = middleware(request("/app/vendors"));
    expect(res.status).toBe(307); // NextResponse.redirect default, not 302
    expect(res.headers.get("location")).toBe("http://localhost/login?next=%2Fapp%2Fvendors");
  });

  it("redirects unauthenticated /admin routes too", () => {
    const res = middleware(request("/admin/users"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("http://localhost/login?next=%2Fadmin%2Fusers");
  });

  it("passes authenticated routes through", () => {
    for (const cookie of [`${ACCESS_COOKIE}=tok`, `${REFRESH_COOKIE}=tok`]) {
      const res = middleware(request("/app/vendors", cookie));
      expect(res.headers.get("x-middleware-next")).toBe("1");
    }
  });

  it("redirects /login away when a session exists (307)", () => {
    const res = middleware(request("/login", `${ACCESS_COOKIE}=tok`));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("http://localhost/app/dashboard");
  });

  it("lets /login through without a session", () => {
    const res = middleware(request("/login"));
    expect(res.headers.get("x-middleware-next")).toBe("1");
  });
});

describe("middleware matcher", () => {
  it("only covers the authenticated areas plus /login", () => {
    expect(config.matcher).toEqual(["/app/:path*", "/admin/:path*", "/login"]);
  });

  it("never matches /api/auth/*, /_next/*, or static assets", () => {
    const prefixes = (config.matcher as string[]).map((pattern) => `/${pattern.split("/")[1]}`);
    for (const path of [
      "/api/auth/login",
      "/api/auth/refresh",
      "/api/auth/switch-tenant",
      "/_next/static/chunk.js",
      "/_next/image",
      "/favicon.ico",
      "/api/v1/vendors",
    ]) {
      expect(prefixes.some((prefix) => path.startsWith(prefix)), `${path} must not match`).toBe(false);
    }
  });
});
