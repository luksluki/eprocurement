import LoginForm from "./LoginForm";
import LanguageSwitcher from "@/components/LanguageSwitcher";
import { getDictionary, getLocale } from "@/i18n/server";

export const dynamic = "force-dynamic";

// /login supports prefill via query params: ?email=… (tenant switcher
// fallback) and ?next=… (post-login redirect target, same-origin only).
// The language switcher lives here so the very first screen is localizable
// without a session.
export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<{ email?: string; next?: string; error?: string }>;
}) {
  const [params, locale, dict] = await Promise.all([searchParams, getLocale(), getDictionary()]);

  return (
    <main className="relative flex min-h-screen items-center justify-center px-4">
      <div className="absolute right-4 top-4">
        <LanguageSwitcher locale={locale} names={dict.localeNames} />
      </div>
      <div className="w-full max-w-sm">
        <div className="mb-8 text-center">
          <div className="mx-auto mb-3 flex h-12 w-12 items-center justify-center rounded-lg bg-slate-900 text-lg font-bold text-white">
            {dict.brand.initials}
          </div>
          <h1 className="text-xl font-semibold">{dict.brand.name}</h1>
          <p className="mt-1 text-sm text-slate-500">{dict.login.subtitle}</p>
        </div>
        <LoginForm
          labels={dict.login}
          initialEmail={params.email ?? ""}
          next={params.next ?? ""}
          switchFailed={params.error === "switch_failed"}
        />
      </div>
    </main>
  );
}
