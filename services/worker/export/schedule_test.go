package export

import (
	"testing"
	"time"
)

func mustSchedule(t *testing.T, spec, tz string) *Daily {
	t.Helper()
	s, err := ParseSchedule(spec, tz)
	if err != nil {
		t.Fatalf("ParseSchedule(%q, %q): %v", spec, tz, err)
	}
	return s
}

// TestParseSchedule pins the stdlib-only parsing: HH:MM in an IANA zone.
// The zone names resolve everywhere because the package embeds
// time/tzdata — exactly what the alpine runtime image needs.
func TestParseSchedule(t *testing.T) {
	cases := []struct {
		name      string
		spec, tz  string
		hour, min int
		loc       string
	}{
		{"default", "01:00", "UTC", 1, 0, "UTC"},
		{"one-digit hour", "1:07", "UTC", 1, 7, "UTC"},
		{"late slot", "23:59", "UTC", 23, 59, "UTC"},
		{"jakarta", "01:00", "Asia/Jakarta", 1, 0, "Asia/Jakarta"},
		{"local", "09:30", "Local", 9, 30, "Local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustSchedule(t, tc.spec, tc.tz)
			if s.hour != tc.hour || s.minute != tc.min || s.Location.String() != tc.loc {
				t.Fatalf("schedule = %+v, want %02d:%02d %s", s, tc.hour, tc.min, tc.loc)
			}
		})
	}
}

func TestParseScheduleErrors(t *testing.T) {
	cases := []struct{ name, spec, tz string }{
		{"no colon", "0100", "UTC"},
		{"two colons", "01:00:00", "UTC"},
		{"hour out of range", "24:00", "UTC"},
		{"minute out of range", "00:60", "UTC"},
		{"negative", "-1:00", "UTC"},
		{"not numeric", "ab:cd", "UTC"},
		{"empty", "", "UTC"},
		{"unknown zone", "01:00", "Mars/Olympus"},
		{"empty zone", "01:00", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSchedule(tc.spec, tc.tz); err == nil {
				t.Fatalf("ParseSchedule(%q, %q) = nil error, want error", tc.spec, tc.tz)
			}
		})
	}
}

// TestNextAndPrevious pin the slot arithmetic: Next is strictly after, in
// the zone's wall clock; Previous is strictly before. Non-UTC zones shift
// the UTC instant — 01:00 Jakarta is 18:00 UTC of the previous day.
func TestNextAndPrevious(t *testing.T) {
	utc := mustSchedule(t, "01:00", "UTC")
	base := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

	if got, want := utc.Next(base), time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
	// Exactly at the slot: next is tomorrow's slot, previous is yesterday's.
	atSlot := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	if got, want := utc.Next(atSlot), time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next at slot = %s, want %s", got, want)
	}
	if got, want := utc.Previous(atSlot), time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Previous at slot = %s, want %s", got, want)
	}
	// Just before the slot: it is the next fire.
	before := time.Date(2026, 10, 28, 0, 59, 59, 0, time.UTC)
	if got, want := utc.Next(before), atSlot; !got.Equal(want) {
		t.Fatalf("Next before slot = %s, want %s", got, want)
	}
	if got, want := utc.Previous(before), time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Previous before slot = %s, want %s", got, want)
	}

	jakarta := mustSchedule(t, "01:00", "Asia/Jakarta")
	// 2026-10-28 01:00 Jakarta (UTC+7) == 2026-10-27 18:00 UTC.
	if got, want := jakarta.Next(base), time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Jakarta Next = %s UTC, want %s UTC", got.UTC(), want.UTC())
	}
	// SlotDate is the zone's calendar date, not raw UTC.
	if got, want := jakarta.SlotDate(base), "2026-10-27"; got != want {
		t.Fatalf("SlotDate = %s, want %s", got, want)
	}
	if got, want := utc.SlotDate(base), "2026-10-27"; got != want {
		t.Fatalf("UTC SlotDate = %s, want %s", got, want)
	}
	// A UTC date that rolls over in Jakarta: 2026-10-28 20:00 UTC is
	// already 2026-10-29 03:00 Jakarta.
	lateUTC := time.Date(2026, 10, 28, 20, 0, 0, 0, time.UTC)
	if got, want := jakarta.SlotDate(lateUTC), "2026-10-29"; got != want {
		t.Fatalf("Jakarta SlotDate = %s, want %s", got, want)
	}
}
