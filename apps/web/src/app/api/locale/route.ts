import { NextResponse } from "next/server";
import { cookies } from "next/headers";
import { LOCALE_COOKIE, isLocale } from "@/i18n";

export const dynamic = "force-dynamic";

const ONE_YEAR_SECONDS = 60 * 60 * 24 * 365;

// POST { locale: "id" | "en" } — persists the UI locale (PRD §19: cookie-based
// ID + EN). This is a route handler because cookies().set() is only legal
// here (same pattern as the /api/auth/* handlers). The one-year maxAge makes
// the choice outlive session cookies and reloads.
export async function POST(request: Request) {
  let body: { locale?: string };
  try {
    body = (await request.json()) as typeof body;
  } catch {
    return NextResponse.json({ error: "Invalid request body." }, { status: 400 });
  }

  const locale = body.locale;
  if (!isLocale(locale)) {
    return NextResponse.json({ error: "Unknown locale." }, { status: 400 });
  }

  const store = await cookies();
  store.set(LOCALE_COOKIE, locale, {
    path: "/",
    sameSite: "lax",
    secure: process.env.NODE_ENV === "production",
    maxAge: ONE_YEAR_SECONDS,
  });
  return NextResponse.json({ ok: true, locale });
}
