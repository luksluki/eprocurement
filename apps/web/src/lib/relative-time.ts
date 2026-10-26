// Relative timestamps for the timeline (issue #43). Pure functions so the
// formatting is unit-testable without the component; `now` is injectable for
// deterministic tests. Intl.RelativeTimeFormat with numeric "auto" yields
// locale-aware output ("3 minutes ago" / "3 menit yang lalu", "now" /
// "sekarang").

export type RelativeTimeLocale = string; // BCP 47 tag, "id" / "en" today

const SECOND = 1000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const WEEK = 7 * DAY;
const MONTH = 30.44 * DAY; // average Gregorian month
const YEAR = 12 * MONTH; // exactly twelve average months, so 12 months = 1 year

// Descending so the first divisor <= |diff| picks the largest fitting unit.
const DIVISORS: ReadonlyArray<{ unit: Intl.RelativeTimeFormatUnit; ms: number }> = [
  { unit: "year", ms: YEAR },
  { unit: "month", ms: MONTH },
  { unit: "week", ms: WEEK },
  { unit: "day", ms: DAY },
  { unit: "hour", ms: HOUR },
  { unit: "minute", ms: MINUTE },
];

// roundHalfAwayFromZero avoids Math.round's half-toward-+∞ behavior, so
// "90 seconds ago" rounds to "2 minutes ago" on both sides of zero.
function roundHalfAwayFromZero(n: number): number {
  return n >= 0 ? Math.round(n) : -Math.round(-n);
}

// formatRelativeTime renders `iso` relative to `now` (default: the current
// time) in `locale`. Unparseable timestamps pass through verbatim — a broken
// value must not blank the timeline row.
export function formatRelativeTime(iso: string, locale: RelativeTimeLocale, now: Date = new Date()): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) {
    return iso;
  }
  const diff = then - now.getTime();
  const rtf = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });

  // Under a minute stays in seconds; rounding to 60 s steps up to a minute.
  if (Math.abs(diff) < MINUTE) {
    const seconds = roundHalfAwayFromZero(diff / SECOND);
    if (Math.abs(seconds) < 60) {
      return rtf.format(seconds, "second");
    }
    return rtf.format(seconds > 0 ? 1 : -1, "minute");
  }

  for (const { unit, ms } of DIVISORS) {
    if (Math.abs(diff) >= ms) {
      return rtf.format(roundHalfAwayFromZero(diff / ms), unit);
    }
  }
  return rtf.format(roundHalfAwayFromZero(diff / MINUTE), "minute");
}
