import type { Metadata } from "next";
import type { ReactNode } from "react";
import { getLocale } from "@/i18n/server";
import "./globals.css";

export const metadata: Metadata = {
  title: "eProcurement",
  description: "Procure-to-pay reference implementation",
};

// <html lang> follows the eproc_locale cookie (PRD §19 i18n). Reading
// cookies() makes the root layout dynamic — every page in this app is
// force-dynamic already (#50), so nothing loses static rendering.
export default async function RootLayout({ children }: { children: ReactNode }) {
  const locale = await getLocale();
  return (
    <html lang={locale}>
      <body className="min-h-screen bg-slate-50 text-slate-900 antialiased">{children}</body>
    </html>
  );
}
