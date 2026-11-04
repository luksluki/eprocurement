import { NextResponse, type NextRequest } from "next/server";
import { ACCESS_COOKIE, REFRESH_COOKIE } from "@/lib/constants";

// Session presence check only. Token VALIDITY is verified by the gateway
// (ADR-0001); an expired access token is handled by the /api/auth/refresh
// redirect dance in the server components, not here.
export function middleware(request: NextRequest) {
  const { pathname } = request.nextUrl;
  const hasSession =
    request.cookies.has(ACCESS_COOKIE) || request.cookies.has(REFRESH_COOKIE);

  if (pathname === "/login") {
    if (hasSession) {
      // NextResponse.redirect defaults to 307.
      return NextResponse.redirect(new URL("/app/dashboard", request.url));
    }
    return NextResponse.next();
  }

  if (!hasSession) {
    const login = new URL("/login", request.url);
    login.searchParams.set("next", pathname);
    return NextResponse.redirect(login);
  }
  return NextResponse.next();
}

export const config = {
  // Include-list matcher: only the authenticated areas plus /login run the
  // middleware. Everything else — /_next/static, /_next/image, /favicon.ico,
  // and crucially /api/auth/* (so login/refresh/switch POSTs are never
  // converted into redirects) — never passes through here.
  matcher: ["/app/:path*", "/admin/:path*", "/login"],
};
