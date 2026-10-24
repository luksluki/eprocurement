import { describe, expect, it } from "vitest";
import { DEFAULT_LOCALE, LOCALE_COOKIE, dictionaries, getDictionary, isLocale } from "./index";

describe("isLocale", () => {
  it.each([
    { value: "id", expected: true },
    { value: "en", expected: true },
    { value: "fr", expected: false },
    { value: "", expected: false },
    { value: undefined, expected: false },
    { value: null, expected: false },
  ])("isLocale($value) === $expected", ({ value, expected }) => {
    expect(isLocale(value)).toBe(expected);
  });
});

describe("dictionaries", () => {
  it("default locale is id per the PRD's ID + EN ordering", () => {
    expect(DEFAULT_LOCALE).toBe("id");
  });

  it.each(Object.keys(dictionaries) as Array<"id" | "en">)("%s dictionary resolves key copy", (locale) => {
    const dict = getDictionary(locale);
    expect(dict.nav.vendors.length).toBeGreaterThan(0);
    expect(dict.login.submit.length).toBeGreaterThan(0);
    expect(dict.vendors.pagination.next.length).toBeGreaterThan(0);
  });

  it("paginates the vendor summary with locale number formatting", () => {
    const range = { from: 1, to: 50, total: 100_000, page: 1, pageCount: 2000 };
    expect(getDictionary("en").vendors.pagination.summary(range)).toBe("Showing 1–50 of 100,000 vendors");
    expect(getDictionary("id").vendors.pagination.summary(range)).toBe(
      "Menampilkan 1–50 dari 100.000 pemasok",
    );
  });

  it("distinguishes copy between locales (the switch is observable)", () => {
    expect(getDictionary("id").nav.vendors).not.toBe(getDictionary("en").nav.vendors);
    expect(getDictionary("id").login.submit).not.toBe(getDictionary("en").login.submit);
  });

  it("names the locale cookie with the eproc_ prefix like the session cookies", () => {
    expect(LOCALE_COOKIE).toBe("eproc_locale");
  });
});
