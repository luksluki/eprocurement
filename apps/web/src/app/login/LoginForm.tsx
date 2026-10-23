"use client";

import { useState, type FormEvent } from "react";

type Props = {
  initialEmail?: string;
  next?: string;
  switchFailed?: boolean;
};

export default function LoginForm({ initialEmail = "", next = "", switchFailed = false }: Props) {
  const [email, setEmail] = useState(initialEmail);
  const [password, setPassword] = useState("");
  const [error, setError] = useState(switchFailed ? "Tenant switch failed. Sign in manually." : "");
  const [busy, setBusy] = useState(false);

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ email, password, next }),
      });
      const data = (await res.json().catch(() => ({}))) as { error?: string; redirect?: string };
      if (!res.ok) {
        setError(data.error ?? "Sign-in failed. Try again.");
        return;
      }
      // Full navigation so the new cookies apply to every request.
      window.location.assign(data.redirect ?? "/app/dashboard");
    } catch {
      setError("Network error — is the gateway running? (GATEWAY_URL)");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      onSubmit={onSubmit}
      className="rounded-xl border border-slate-200 bg-white p-6 shadow-sm"
      aria-labelledby="login-heading"
      noValidate={false}
    >
      <h2 id="login-heading" className="sr-only">
        Sign in
      </h2>
      <label htmlFor="email" className="block text-sm font-medium text-slate-700">
        Email
      </label>
      <input
        id="email"
        name="email"
        type="email"
        autoComplete="username"
        required
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        className="mt-1 w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-slate-900 focus:outline-none"
      />
      <label htmlFor="password" className="mt-4 block text-sm font-medium text-slate-700">
        Password
      </label>
      <input
        id="password"
        name="password"
        type="password"
        autoComplete="current-password"
        required
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        className="mt-1 w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-slate-900 focus:outline-none"
      />
      {error ? (
        <p role="alert" data-testid="login-error" className="mt-4 rounded-md bg-red-50 px-3 py-2 text-sm text-red-700">
          {error}
        </p>
      ) : null}
      <button
        type="submit"
        disabled={busy}
        className="mt-6 w-full rounded-md bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:opacity-60"
      >
        {busy ? "Signing in…" : "Sign in"}
      </button>
    </form>
  );
}
