import LoginForm from "./LoginForm";

export const dynamic = "force-dynamic";

// /login supports prefill via query params: ?email=… (tenant switcher
// fallback) and ?next=… (post-login redirect target, same-origin only).
export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<{ email?: string; next?: string; error?: string }>;
}) {
  const params = await searchParams;
  return (
    <main className="flex min-h-screen items-center justify-center px-4">
      <div className="w-full max-w-sm">
        <div className="mb-8 text-center">
          <div className="mx-auto mb-3 flex h-12 w-12 items-center justify-center rounded-lg bg-slate-900 text-lg font-bold text-white">
            eP
          </div>
          <h1 className="text-xl font-semibold">eProcurement</h1>
          <p className="mt-1 text-sm text-slate-500">Sign in to your workspace</p>
        </div>
        <LoginForm initialEmail={params.email ?? ""} next={params.next ?? ""} switchFailed={params.error === "switch_failed"} />
      </div>
    </main>
  );
}
