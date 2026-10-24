// Server-only i18n helpers: read the eproc_locale cookie and resolve the
// dictionary. Server components are the only consumers; client components
// receive copy via props.

import { cookies } from "next/headers";
import { DEFAULT_LOCALE, LOCALE_COOKIE, dictionaries, isLocale } from "./index";
import type { Dictionary, Locale } from "./index";

export type { Dictionary, Locale };

export async function getLocale(): Promise<Locale> {
  const store = await cookies();
  const value = store.get(LOCALE_COOKIE)?.value;
  return isLocale(value) ? value : DEFAULT_LOCALE;
}

// getDictionary() with no argument resolves the cookie locale; tests use the
// pure getDictionary(locale) from "@/i18n".
export async function getDictionary(): Promise<Dictionary> {
  return dictionaries[await getLocale()];
}
