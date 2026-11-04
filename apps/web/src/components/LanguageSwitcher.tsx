"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { Button } from "@eprocurement/ui";
import { LOCALES, type Locale } from "@/i18n";

type Props = {
  locale: Locale;
  // Accessible names per locale (e.g. "Bahasa Indonesia" / "English"); the
  // button labels themselves stay the short codes ID/EN.
  names: Record<Locale, string>;
};

// Cookie-based locale switcher (PRD §19): POSTs the choice to /api/locale,
// which writes the eproc_locale cookie in a route handler (the only legal
// place for cookies().set()), then refreshes the server-rendered tree. The
// cookie — not client state — is the source of truth, so the choice survives
// reloads and navigation.
export default function LanguageSwitcher({ locale, names }: Props) {
  const router = useRouter();
  const [busy, setBusy] = useState<Locale | null>(null);

  async function switchTo(next: Locale) {
    if (next === locale || busy !== null) {
      return;
    }
    setBusy(next);
    try {
      await fetch("/api/locale", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ locale: next }),
      });
    } catch {
      // Keep the current locale on failure; the cookie is unchanged.
    } finally {
      setBusy(null);
      router.refresh();
    }
  }

  return (
    <div className="flex items-center gap-1">
      {LOCALES.map((option) => (
        <Button
          key={option}
          variant={option === locale ? "primary" : "secondary"}
          size="sm"
          aria-pressed={option === locale}
          aria-label={names[option]}
          disabled={busy !== null}
          onClick={() => void switchTo(option)}
        >
          {option.toUpperCase()}
        </Button>
      ))}
    </div>
  );
}
