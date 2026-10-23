"use client";

import { useState } from "react";

// Signs out via the logout route handler (which clears the httpOnly cookies
// server-side), then navigates to /login with a full reload.
export default function LogoutButton() {
  const [busy, setBusy] = useState(false);

  async function onClick() {
    setBusy(true);
    try {
      await fetch("/api/auth/logout", { method: "POST" });
    } catch {
      // Cookies are cleared best-effort server-side; navigate regardless.
    } finally {
      window.location.assign("/login");
    }
  }

  return (
    <button
      type="button"
      onClick={() => void onClick()}
      disabled={busy}
      className="rounded-md border border-slate-300 px-3 py-1 text-sm text-slate-700 hover:bg-slate-50 disabled:opacity-60"
    >
      {busy ? "Signing out…" : "Sign out"}
    </button>
  );
}
