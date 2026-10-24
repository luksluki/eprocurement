"use client";

import { useState } from "react";
import { Button } from "@eprocurement/ui";

export type LogoutLabels = {
  signOut: string;
  signingOut: string;
};

// Signs out via the logout route handler (which clears the httpOnly cookies
// server-side), then navigates to /login with a full reload. Button markup
// comes from the design system.
export default function LogoutButton({ labels }: { labels: LogoutLabels }) {
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
    <Button variant="secondary" size="sm" disabled={busy} onClick={() => void onClick()}>
      {busy ? labels.signingOut : labels.signOut}
    </Button>
  );
}
