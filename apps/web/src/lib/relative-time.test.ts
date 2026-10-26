// formatRelativeTime unit tests (issue #43): deterministic `now`, both UI
// locales (id/en, PRD §19). The pinned strings are exactly what Node's
// Intl.RelativeTimeFormat(numeric: "auto") emits — including the word forms
// ("last month", "bulan lalu") for singular month/year spans.
import { describe, expect, it } from "vitest";
import { formatRelativeTime } from "./relative-time";

const NOW = new Date("2026-10-26T12:00:00Z");

describe("formatRelativeTime", () => {
  it("renders 'now' in both locales for the current instant", () => {
    expect(formatRelativeTime("2026-10-26T12:00:00Z", "id", NOW)).toBe("sekarang");
    expect(formatRelativeTime("2026-10-26T12:00:00Z", "en", NOW)).toBe("now");
  });

  it("formats past and future spans in id and en", () => {
    const cases = [
      { iso: "2026-10-26T11:59:35Z", id: "25 detik yang lalu", en: "25 seconds ago" },
      { iso: "2026-10-26T11:56:00Z", id: "4 menit yang lalu", en: "4 minutes ago" },
      { iso: "2026-10-26T09:00:00Z", id: "3 jam yang lalu", en: "3 hours ago" },
      { iso: "2026-10-23T12:00:00Z", id: "3 hari yang lalu", en: "3 days ago" },
      { iso: "2026-10-12T12:00:00Z", id: "2 minggu yang lalu", en: "2 weeks ago" },
      { iso: "2026-09-21T12:00:00Z", id: "bulan lalu", en: "last month" },
      { iso: "2025-09-21T12:00:00Z", id: "tahun lalu", en: "last year" },
      { iso: "2026-10-26T12:00:20Z", id: "dalam 20 detik", en: "in 20 seconds" },
    ];
    for (const { iso, id, en } of cases) {
      expect(formatRelativeTime(iso, "id", NOW), `${iso} id`).toBe(id);
      expect(formatRelativeTime(iso, "en", NOW), `${iso} en`).toBe(en);
    }
  });

  it("steps up when second-rounding crosses the minute boundary", () => {
    // 59.6 s before NOW rounds to 60 s and must render as one minute.
    expect(formatRelativeTime("2026-10-26T11:59:00.4Z", "en", NOW)).toBe("1 minute ago");
  });

  it("rounds half-units away from zero", () => {
    // 90 s is 1.5 minutes → "2 minutes ago", not Math.round(-1.5) = 1.
    expect(formatRelativeTime("2026-10-26T11:58:30Z", "en", NOW)).toBe("2 minutes ago");
  });

  it("passes unparseable timestamps through instead of blanking the row", () => {
    expect(formatRelativeTime("not-a-date", "en", NOW)).toBe("not-a-date");
    expect(formatRelativeTime("", "id", NOW)).toBe("");
  });
});
