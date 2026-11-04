import { NextResponse } from "next/server";
import { gatewayLogout } from "@/lib/gateway";
import { clearSessionCookies, REFRESH_COOKIE } from "@/lib/session";
import { cookies } from "next/headers";

export const dynamic = "force-dynamic";

// POST /api/auth/logout — revokes the refresh token at the gateway
// (best-effort) and clears the session cookies. Sign-out must succeed even
// when the gateway is down.
export async function POST() {
  const store = await cookies();
  const refreshToken = store.get(REFRESH_COOKIE)?.value;
  if (refreshToken) {
    await gatewayLogout(refreshToken);
  }
  await clearSessionCookies();
  return NextResponse.json({ ok: true, redirect: "/login" });
}
