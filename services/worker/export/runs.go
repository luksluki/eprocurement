package export

import (
	"context"
	"log/slog"
	"time"

	"github.com/luksluki/eprocurement/services/documents/export"
)

// RunStore is the run-history write/read surface the tracker needs; the
// documents export store implements it (the worker projects, documents
// owns — the #47 notifications precedent).
type RunStore interface {
	UpsertRun(ctx context.Context, run export.Run) error
	Latest(ctx context.Context, tenantID string) (*export.Run, error)
	HasSuccess(ctx context.Context, tenantID string, from, to time.Time) (bool, error)
	MarkNewestMissedPrevious(ctx context.Context, tenantID string) error
}

// Detection reasons, surfaced in the warning log.
const (
	reasonHeartbeatStale = "heartbeat_stale"
	reasonSlotEmpty      = "previous_slot_missed"
)

// Tracker owns the run lifecycle around one tenant's export: missed-run
// detection (PRD §14.8 "Restart → missed-run detection alerts"), the
// success heartbeat, and the run record. Detection runs at scheduler boot
// and again at each fire for the lock winner — never as catch-up: a missed
// slot stays missed (#53 owns manual execution).
type Tracker struct {
	kv       KV
	runs     RunStore
	schedule *Daily
	log      *slog.Logger
	now      func() time.Time
}

// NewTracker binds the tracker to its Redis KV, run store, and schedule.
func NewTracker(kv KV, runs RunStore, schedule *Daily, log *slog.Logger) *Tracker {
	return &Tracker{kv: kv, runs: runs, schedule: schedule, log: log, now: time.Now}
}

// Detect answers whether a slot was missed for the tenant as of now. The
// first-ever run is the cold-start exception (no run records → not
// missed). A miss is declared when the heartbeat is older than
// MissedGrace (26 h) OR the previous slot holds no successful run — the
// half-open slot range tolerates late-waking fires. Detection failures
// (Redis/Mongo down) degrade to "not missed": the export itself still
// runs, and its own outcome is recorded either way.
func (t *Tracker) Detect(ctx context.Context, tenantID string, now time.Time) (missed bool, reason string) {
	latest, err := t.runs.Latest(ctx, tenantID)
	if err != nil {
		t.log.Warn("export detection could not read run history",
			"tenant_id", tenantID, "error", err)
		return false, ""
	}
	if latest == nil {
		return false, "" // cold start: nothing has ever run for this tenant
	}
	if value, ok, err := t.kv.Get(ctx, HeartbeatKey(tenantID)); err != nil {
		t.log.Warn("export detection could not read heartbeat",
			"tenant_id", tenantID, "error", err)
	} else if ok {
		if last, err := ParseHeartbeat(value); err != nil {
			t.log.Warn("export detection found a malformed heartbeat",
				"tenant_id", tenantID)
		} else if age := heartbeatAge(now, last); age > MissedGrace {
			return true, reasonHeartbeatStale
		}
	}
	prev := t.schedule.Previous(now)
	if ok, err := t.runs.HasSuccess(ctx, tenantID, prev, prev.Add(24*time.Hour)); err != nil {
		t.log.Warn("export detection could not read previous slot",
			"tenant_id", tenantID, "error", err)
	} else if !ok {
		return true, reasonSlotEmpty
	}
	return false, ""
}

// DetectAndFlag is the boot pass: detect, then surface the miss without
// waiting for the next fire by flagging the tenant's newest run record
// (missed_previous=true) and logging the warning.
func (t *Tracker) DetectAndFlag(ctx context.Context, tenantID string, now time.Time) {
	missed, reason := t.Detect(ctx, tenantID, now)
	if !missed {
		return
	}
	if err := t.runs.MarkNewestMissedPrevious(ctx, tenantID); err != nil {
		t.log.Warn("export detection could not flag the run record",
			"tenant_id", tenantID, "error", err)
	}
	t.warnMissed(tenantID, reason, now)
}

// warnMissed is the PRD §14.8 alert (the metric lands with #55): one
// structured warning line per detected miss.
func (t *Tracker) warnMissed(tenantID, reason string, now time.Time) {
	t.log.Warn("export missed run detected",
		"tenant_id", tenantID,
		"reason", reason,
		"grace", MissedGrace.String(),
		"detected_at", now.UTC().Format(time.RFC3339))
}

// RecordSuccess writes the run record for a successful export and stamps
// the heartbeat (PRD §16: heartbeat updated on success). The record's
// missed_previous carries the fire-time detection result.
func (t *Tracker) RecordSuccess(ctx context.Context, tenantID string, run export.Run) error {
	if err := t.runs.UpsertRun(ctx, run); err != nil {
		return err
	}
	if err := t.kv.Set(ctx, HeartbeatKey(tenantID), FormatHeartbeat(t.now())); err != nil {
		// The record is durable; a heartbeat write failure only degrades
		// the next detection pass (which still consults the slot history).
		t.log.Warn("export heartbeat write failed", "tenant_id", tenantID, "error", err)
	}
	return nil
}

// RecordFailure writes the failed run record — status=failed plus the
// error message, the basis of #53's error display. No heartbeat is
// touched: failures do not reset the 26 h clock.
func (t *Tracker) RecordFailure(ctx context.Context, run export.Run) error {
	return t.runs.UpsertRun(ctx, run)
}
