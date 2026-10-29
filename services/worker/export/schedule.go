// Package export is the worker's nightly CSV export (#49, FR-10, PRD §16):
// one env-level schedule (EXPORT_SCHEDULE + EXPORT_TZ), tenants enumerated
// from procurement.purchase_orders, a per-tenant Redis distributed lock
// (lock:export:nightly:{tenant}), a PO register CSV per tenant written to
// MinIO under exports/{tenant}/{yyyy-mm-dd}/purchase-orders.csv, a success
// heartbeat (job:export:last_success:{tenant}), and missed-run detection
// (heartbeat older than 26 h or an empty previous slot → the run history
// records missed_previous and a warning is logged). Overlap is prevented,
// not repaired: a skipped slot is never re-run (#53 owns manual runs).
package export

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// The runtime image is alpine:3.20 without the tzdata package
	// (deploy/docker/service.Dockerfile), so IANA zone data must come from
	// the binary itself — without this blank import, LoadLocation succeeds
	// only for "UTC"/"Local" inside the container and any other EXPORT_TZ
	// fails scheduler construction at boot.
	_ "time/tzdata"
)

// Daily is one wall-clock fire time per day in one IANA zone — EXPORT_SCHEDULE
// (HH:MM) + EXPORT_TZ, parsed with the standard library only (maintainer
// decision 2026-10-27: no cron library).
type Daily struct {
	Location *time.Location
	hour     int
	minute   int
}

// ParseSchedule parses "HH:MM" in tzSpec (an IANA zone name, "UTC", or
// "Local"). Both parts must be present and in range; the colon is the only
// separator. Values like "1:07" parse (one-digit hour) like every cron
// dialect. An empty zone is rejected explicitly — LoadLocation("") would
// silently mean UTC, and a misconfigured EXPORT_TZ should stop boot.
func ParseSchedule(spec, tzSpec string) (*Daily, error) {
	parts := strings.Split(strings.TrimSpace(spec), ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("export: schedule %q must be HH:MM", spec)
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("export: schedule hour %q: %w", parts[0], err)
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("export: schedule minute %q: %w", parts[1], err)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return nil, fmt.Errorf("export: schedule %q out of range", spec)
	}
	zone := strings.TrimSpace(tzSpec)
	if zone == "" {
		return nil, fmt.Errorf("export: timezone must not be empty")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("export: timezone %q: %w", tzSpec, err)
	}
	return &Daily{Location: loc, hour: hour, minute: minute}, nil
}

// Next returns the next fire time strictly after `after`, as a wall-clock
// instant in the schedule's zone. AddDate performs calendar arithmetic, so
// the wall time survives DST transitions the way a 01:00 cron would.
func (s *Daily) Next(after time.Time) time.Time {
	a := after.In(s.Location)
	candidate := time.Date(a.Year(), a.Month(), a.Day(), s.hour, s.minute, 0, 0, s.Location)
	if !candidate.After(after) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// Previous returns the latest fire time strictly before `before` — the slot
// the missed-run detection asks about. At a fire instant it is the day's
// predecessor; between fires it is the most recent slot that should already
// have run.
func (s *Daily) Previous(before time.Time) time.Time {
	b := before.In(s.Location)
	candidate := time.Date(b.Year(), b.Month(), b.Day(), s.hour, s.minute, 0, 0, s.Location)
	if !candidate.Before(before) {
		candidate = candidate.AddDate(0, 0, -1)
	}
	return candidate
}

// SlotDate is the {yyyy-mm-dd} segment of the export object key: the fire
// time's calendar date in the schedule's zone, not raw UTC (maintainer
// decision: a 01:00 Asia/Jakarta run belongs to the Jakarta date).
func (s *Daily) SlotDate(fireTime time.Time) string {
	return fireTime.In(s.Location).Format("2006-01-02")
}

// MissedGrace is how long a stale heartbeat may get before a miss is
// declared: one 24 h slot plus a 2 h grace for slow runs and clock skew
// (PRD §16: "alert if > 26 h").
const MissedGrace = 26 * time.Hour
