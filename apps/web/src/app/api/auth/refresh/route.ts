import { NextResponse } from "next/server";
import { refreshPair, RefreshFailed } from "@/lib/refresh-singleflight";
import { safeNext } from "@/lib/redirect";
import { clearSessionCookies, REFRESH_COOKIE, setSessionCookies } from "@/lib/session";
import { cookies } from "next/headers";

export const dynamic = "force-dynamic";

// GET /api/auth/refresh?next=<path> — the redirect dance. Server components
// send expired sessions here as a browser navigation; this route handler is
// allowed to rotate the cookies, then bounces the user back. On failure the
// session is gone: clear everything and land on /login with the original
// target preserved. 303 so the browser follows up with GET.
export async function GET(request: Request) {
  const url = new URL(request.url);
  const next = safeNext(url.searchParams.get("next"));

  const store = await cookies();
  const refreshToken = store.get(REFRESH_COOKIE)?.value;
  if (!refreshToken) {
    await clearSessionCookies();
    return seeOther(request, `/login?next=${encodeURIComponent(next)}`);
  }

  try {
    const pair = await refreshPair(refreshToken);
    await setSessionCookies(pair);
    return seeOther(request, next);
  } catch (error) {
    if (!(error instanceof RefreshFailed)) {
      // Unexpected shape — treat like a lost session rather than crashing.
      await clearSessionCookies();
      return seeOther(request, `/login?next=${encodeURIComponent(next)}`);
    }
    if (error.status === 401) {
      // Rotated/revoked/expired refresh token: the session is gone.
      await clearSessionCookies();
      return seeOther(request, `/login?next=${encodeURIComponent(next)}`);
    }
    // Gateway unreachable (502/5xx): keep the cookies — the refresh token may
    // still be valid once the gateway is back — and send to /login, which
    // offers a way forward without destroying the session.
    return seeOther(request, `/login?next=${encodeURIComponent(next)}`);
  }
}

function seeOther(request: Request, path: string): NextResponse {
  return NextResponse.redirect(new URL(path, new URL(request.url).origin), 303);
}
