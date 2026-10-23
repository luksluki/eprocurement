import { NextResponse } from "next/server";
import { gatewayLogin } from "@/lib/gateway";
import { safeNext } from "@/lib/redirect";
import { setSessionCookies } from "@/lib/session";

export const dynamic = "force-dynamic";

// POST /api/auth/login — the only place (besides refresh/switch) that mints
// session cookies: route handlers may call cookies().set(), server components
// cannot. Credentials go to the gateway's public login route; the tokens
// never surface to the browser beyond httpOnly cookies.
export async function POST(request: Request) {
  let body: { email?: string; password?: string; next?: string };
  try {
    body = (await request.json()) as typeof body;
  } catch {
    return NextResponse.json({ error: "Invalid request body." }, { status: 400 });
  }
  const email = (body.email ?? "").trim();
  const password = body.password ?? "";
  if (!email || !password) {
    return NextResponse.json({ error: "Email and password are required." }, { status: 400 });
  }

  const result = await gatewayLogin(email, password);
  if (!result.ok) {
    const message =
      result.status === 401
        ? "Invalid email or password."
        : result.status === 429
          ? "Too many login attempts. Try again in a minute."
          : "Sign-in is unavailable right now. Try again shortly.";
    const status = result.status === 400 || result.status === 401 || result.status === 429 ? result.status : 502;
    return NextResponse.json({ error: message }, { status });
  }

  await setSessionCookies(result.pair);
  return NextResponse.json({ ok: true, redirect: safeNext(body.next) });
}
