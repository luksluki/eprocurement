import { NextResponse } from "next/server";
import { parseDemoTenants } from "@/lib/demo-tenants";
import { gatewayLogin } from "@/lib/gateway";
import { safeNext } from "@/lib/redirect";
import { setSessionCookies } from "@/lib/session";

export const dynamic = "force-dynamic";

// POST /api/auth/switch-tenant?next=<path> {tenant: <slug>} — demo tenant
// switcher (maintainer decision 2026-10-23 on issue #50): re-login server-side
// as the target tenant's admin with DEMO_ADMIN_PASSWORD, rotating the session
// cookies legally inside this route handler. Without a stored password the
// switcher falls back to /login prefilled with the target admin's email.
// scripts/seed is never touched (demo-schema isolation, onboarding §5).
export async function POST(request: Request) {
  const url = new URL(request.url);
  const next = safeNext(url.searchParams.get("next"));

  let body: { tenant?: string };
  try {
    body = (await request.json()) as typeof body;
  } catch {
    return NextResponse.json({ error: "Invalid request body." }, { status: 400 });
  }
  const slug = (body.tenant ?? "").trim();

  const target = parseDemoTenants(process.env.DEMO_TENANTS).find((t) => t.slug === slug);
  if (!target) {
    return NextResponse.json({ error: "Unknown demo tenant." }, { status: 400 });
  }

  const password = process.env.DEMO_ADMIN_PASSWORD ?? "";
  if (!password) {
    return NextResponse.json({
      ok: false,
      redirect: `/login?email=${encodeURIComponent(target.email)}`,
    });
  }

  const result = await gatewayLogin(target.email, password);
  if (!result.ok) {
    // Wrong shared password, or identity's per-email limiter tripped
    // (rapid switching; see docs/onboarding.md) — hand over to manual login.
    return NextResponse.json({
      ok: false,
      redirect: `/login?email=${encodeURIComponent(target.email)}&error=switch_failed`,
    });
  }

  await setSessionCookies(result.pair);
  return NextResponse.json({ ok: true, redirect: next });
}
