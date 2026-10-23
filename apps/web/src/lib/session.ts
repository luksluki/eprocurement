// Session hydration for server components + cookie rotation for route
// handlers. Cookies are written exclusively in route handlers (the only place
// Next.js allows cookies().set()); server components read and redirect.

import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { ACCESS_COOKIE, REFRESH_COOKIE } from "./constants";
import { fetchMe, type Claims, type TokenPair } from "./gateway";
import { tokenNeedsRefresh } from "./token-exp";

export { ACCESS_COOKIE, REFRESH_COOKIE };
export type { Claims };

export type Session = {
  claims: Claims;
  accessToken: string;
};

// refreshHref is the redirect-dance entry: a browser navigation that (legally)
// rotates the cookies in a route handler and bounces back.
export function refreshHref(pathname: string): string {
  return `/api/auth/refresh?next=${encodeURIComponent(pathname)}`;
}

// requireSession is the gate for authenticated server components: no session
// → /login; expired access token or a gateway 401 → the refresh dance; then
// hydrated claims + the live access token for downstream gateway calls.
export async function requireSession(pathname: string): Promise<Session> {
  const store = await cookies();
  const accessToken = store.get(ACCESS_COOKIE)?.value;
  const refreshToken = store.get(REFRESH_COOKIE)?.value;

  if (!accessToken && !refreshToken) {
    redirect(`/login?next=${encodeURIComponent(pathname)}`);
  }
  if (!accessToken || tokenNeedsRefresh(accessToken)) {
    redirect(refreshHref(pathname));
  }
  const claims = await fetchMe(accessToken);
  if (!claims) {
    redirect(refreshHref(pathname));
  }
  return { claims, accessToken };
}

// setSessionCookies rotates the session cookies. Route handlers only.
export async function setSessionCookies(pair: TokenPair): Promise<void> {
  const store = await cookies();
  const secure = process.env.NODE_ENV === "production";
  store.set(ACCESS_COOKIE, pair.access_token, {
    httpOnly: true,
    sameSite: "lax",
    secure,
    path: "/",
    maxAge: pair.access_expires_in,
  });
  store.set(REFRESH_COOKIE, pair.refresh_token, {
    httpOnly: true,
    sameSite: "lax",
    secure,
    path: "/",
    maxAge: pair.refresh_expires_in,
  });
}

// clearSessionCookies drops the session. Route handlers only.
export async function clearSessionCookies(): Promise<void> {
  const store = await cookies();
  store.delete(ACCESS_COOKIE);
  store.delete(REFRESH_COOKIE);
}
