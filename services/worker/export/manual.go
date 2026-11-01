package export

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/luksluki/eprocurement/services/documents/export"
	"github.com/luksluki/eprocurement/services/documents/history"
)

// manualTimeoutMargin pads the manual runner's consumer DeliveryTimeout
// beyond job budget + record budget: the runner's own context.WithTimeout
// must never be what cancels a delivery whose work still fits its budget —
// the margin absorbs scheduler jitter between the two deadlines.
const manualTimeoutMargin = 30 * time.Second

// ManualDeliveryTimeout sizes the consumer runner's DeliveryTimeout for the
// exports queue (#53): the job budget plus the record-write budget plus a
// margin. The #47 runner's default cap is 10 s — far below the default
// EXPORT_JOB_BUDGET of 10 m — which is exactly why the exports queue gets
// its OWN runner (services/worker/cmd/worker): the history/notifications
// guarantees stay untouched. Fails fast on a non-positive budget, the
// NewScheduler convention.
func ManualDeliveryTimeout(budget time.Duration) (time.Duration, error) {
	if budget <= 0 {
		return 0, fmt.Errorf("export: manual delivery timeout: job budget must be positive, got %s", budget)
	}
	return budget + RecordWriteBudget + manualTimeoutMargin, nil
}

// ManualRunner consumes export.requested events (#53, FR-10's manual half;
// PRD §14.5 queue `exports`) and runs one manual export per delivery. It
// implements consumer.Handler: the runner decodes and validates the §14.5
// envelope, then calls Apply with the event.
//
// Semantics:
//   - the export window is the trailing 24 h BEFORE the event's
//     occurred_at, half-open [occurredAt−24h, occurredAt) — anchored on the
//     event, not the consume time, so broker retries of the same delivery
//     always export the same window;
//   - the object key is the manual twin of the nightly one,
//     exports/{tenant}/{yyyy-mm-dd}/purchase-orders-manual-{HHMMSS}.csv
//     with date and clock in EXPORT_TZ (the issue #53 decision: the dated
//     nightly artifact is never overwritten). KNOWN EDGE, accepted by the
//     same decision: two triggers within one second share the key and the
//     later run's bytes win (last-write-wins on the object) while both run
//     records exist — documented in docs/schema/documents.md;
//   - the run record carries trigger="manual" and scheduled_for =
//     occurred_at, so the {tenant_id, scheduled_for} upsert is the retry
//     idempotency: a redelivery overwrites the same record, never appends;
//   - NO Redis lock and NO heartbeat/missed-run interaction: manual runs
//     are single deliveries on a prefetch-1 queue, and they never reset
//     the nightly 26 h detection clock (the run store scopes detection to
//     scheduled runs);
//   - a failure writes status=failed + error_message on a FRESH context
//     (WithoutCancel — the record must land even when the budget or the
//     runner deadline was what expired) and still returns the error, so
//     the broker's retry ladder (5 s/30 s/5 m → DLQ) applies.
//
// export.completed/export.failed are deliberately NOT emitted (the #49
// documented gap, kept by the #53 decision: the worker must not write the
// procurement outbox; the Mongo run history is the record).
type ManualRunner struct {
	register ManualRegister
	objects  ObjectStore
	runs     RunStore
	loc      *time.Location // anchors the object key's date + clock in EXPORT_TZ
	budget   time.Duration
	log      *slog.Logger
	now      func() time.Time
}

// ManualRegister reads one tenant's PO register slice for a window — the
// concrete Register satisfies it (the TenantLister/TenantExporter pattern
// of scheduler.go; the interface keeps the runner unit-testable).
type ManualRegister interface {
	PurchaseOrders(ctx context.Context, tenantID string, from, to time.Time) ([]PurchaseOrderRow, error)
}

// ManualConfig carries the manual runner's env-level knobs — the same
// EXPORT_TZ and EXPORT_JOB_BUDGET strings services/worker/config parsed.
type ManualConfig struct {
	Timezone string        // EXPORT_TZ (object key date + clock)
	Budget   time.Duration // EXPORT_JOB_BUDGET bounding the export itself
}

// NewManualRunner validates the configuration fail-fast (non-positive
// budget, malformed zone — the NewScheduler convention) and binds the
// dependencies.
func NewManualRunner(cfg ManualConfig, register ManualRegister, objects ObjectStore,
	runs RunStore, log *slog.Logger) (*ManualRunner, error) {
	if cfg.Budget <= 0 {
		return nil, fmt.Errorf("export: manual budget must be positive, got %s", cfg.Budget)
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("export: manual zone: %w", err)
	}
	return &ManualRunner{
		register: register,
		objects:  objects,
		runs:     runs,
		loc:      loc,
		budget:   cfg.Budget,
		log:      log,
		now:      time.Now,
	}, nil
}

// ManualObjectKey is the pinned manual key: exports/{tenant_id}/
// {yyyy-mm-dd}/purchase-orders-manual-{HHMMSS}.csv with the date AND the
// clock from the anchor time in EXPORT_TZ. Unlike the nightly key, the
// clock segment makes successive manual runs distinct; only same-second
// triggers collide (the documented known edge).
func ManualObjectKey(tenantID string, loc *time.Location, anchor time.Time) string {
	local := anchor.In(loc)
	return fmt.Sprintf("exports/%s/%s/purchase-orders-manual-%s.csv",
		tenantID, local.Format("2006-01-02"), local.Format("150405"))
}

// Apply runs one manual export for the event's tenant. The bool reports
// whether THIS call did the work (true on success — the upsert-keyed
// record makes the redelivery of the same event an overwrite, so there is
// no separate duplicate signal).
func (h *ManualRunner) Apply(ctx context.Context, ev history.Event) (bool, error) {
	if ev.Type != EventRequestedType {
		// The exports queue binds export.requested only; anything else is
		// topology drift. Not a history validation sentinel, so the
		// consumer treats it as transient: it climbs the retry ladder and
		// dead-letters visibly instead of being acked silently.
		return false, fmt.Errorf("export: manual runner got %q on the exports queue; want %s", ev.Type, EventRequestedType)
	}

	started := h.now()
	runCtx, cancel := context.WithTimeout(ctx, h.budget)
	defer cancel()

	rowCount, objectKey, exportErr := h.export(runCtx, ev.TenantID, ev.OccurredAt)
	finished := h.now()

	run := newManualRun(ev, started, finished)

	// The runOwned pattern: the record is written on a fresh context — the
	// budget (or the runner's DeliveryTimeout, or SIGTERM) may be exactly
	// what failed the run, and a failed run must still be recorded (its
	// message is the reports page's error display).
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), RecordWriteBudget)
	defer recordCancel()

	if exportErr != nil {
		run.Status = export.StatusFailed
		run.ErrorMessage = exportErr.Error()
		h.log.Error("manual export run failed",
			"tenant_id", ev.TenantID, "event_id", ev.EventID, "error", exportErr)
		if err := h.runs.UpsertRun(recordCtx, run); err != nil {
			h.log.Error("manual export failed-run record write failed",
				"tenant_id", ev.TenantID, "event_id", ev.EventID, "error", err)
		}
		return false, exportErr // transient for the consumer: retry ladder applies
	}

	run.Status = export.StatusSuccess
	run.RowCount = rowCount
	run.ObjectKey = objectKey
	h.log.Info("manual export run succeeded",
		"tenant_id", ev.TenantID, "event_id", ev.EventID,
		"rows", rowCount, "object_key", objectKey)
	if err := h.runs.UpsertRun(recordCtx, run); err != nil {
		// The artifact is durable in MinIO; only the history write failed.
		// Return the error so the broker redelivers — the upsert key makes
		// the retry idempotent on the record side.
		h.log.Error("manual export run record write failed",
			"tenant_id", ev.TenantID, "event_id", ev.EventID, "error", err)
		return false, err
	}
	return true, nil
}

// export runs the trailing-24 h register anchored on the event's
// occurred_at and stores the CSV under the manual key.
func (h *ManualRunner) export(ctx context.Context, tenantID string, anchor time.Time) (int, string, error) {
	rows, err := h.register.PurchaseOrders(ctx, tenantID, anchor.Add(-24*time.Hour), anchor)
	if err != nil {
		return 0, "", err
	}
	key := ManualObjectKey(tenantID, h.loc, anchor)
	if err := putCSV(ctx, h.objects, key, rows); err != nil {
		return 0, "", err
	}
	return len(rows), key, nil
}

// newManualRun assembles the manual run record: trigger=manual,
// scheduled_for = the event's occurred_at (the {tenant_id, scheduled_for}
// upsert key — deterministic across broker retries).
func newManualRun(ev history.Event, started, finished time.Time) export.Run {
	return export.Run{
		RunID:        uuid.NewString(),
		TenantID:     ev.TenantID,
		ScheduledFor: ev.OccurredAt,
		Trigger:      export.TriggerManual,
		StartedAt:    started,
		FinishedAt:   finished,
	}
}

// putCSV encodes the register rows and stores them under key (the
// Exporter.Export body — shared with the nightly path's conventions).
func putCSV(ctx context.Context, objects ObjectStore, key string, rows []PurchaseOrderRow) error {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows); err != nil {
		return fmt.Errorf("export: csv encode: %w", err)
	}
	if err := objects.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), CSVContentType); err != nil {
		return fmt.Errorf("export: store %s: %w", key, err)
	}
	return nil
}

// EventRequestedType is the §14.6 catalog name this runner consumes (the
// procurement export package's EventRequested twin — restated locally so
// the worker does not import the procurement service).
const EventRequestedType = "export.requested"
