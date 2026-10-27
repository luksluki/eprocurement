package export

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/services/documents/export"
)

// Default configuration values (#49). The lock TTL must strictly exceed the
// job budget so a lock never expires while its holder may still be working
// — NewScheduler rejects anything else at boot rather than letting two
// schedulers overlap silently.
const (
	DefaultSchedule   = "01:00"
	DefaultTimezone   = "UTC"
	DefaultLockTTL    = 15 * time.Minute
	DefaultJobBudget  = 10 * time.Minute
	recordWriteBudget = 10 * time.Second
)

// TenantLister enumerates the tenants to export (the PO register).
type TenantLister interface {
	Tenants(ctx context.Context) ([]string, error)
}

// TenantExporter produces one tenant's CSV for one fire.
type TenantExporter interface {
	Export(ctx context.Context, tenantID string, fireTime time.Time) (rowCount int, objectKey string, err error)
}

// Scheduler runs the nightly export: on boot it sweeps the missed-run
// detection once per tenant, then fires every tenant's export at the
// scheduled wall-clock time in the configured zone. Per tenant, the Redis
// lock decides which instance owns the run: the loser skips (log only, no
// run record — the run belongs to the winner); the winner detects, exports
// under the job budget, records the outcome, and releases.
type Scheduler struct {
	schedule  *Daily
	lockTTL   time.Duration
	budget    time.Duration
	tenants   TenantLister
	exporter  TenantExporter
	tracker   *Tracker
	kv        KV
	log       *slog.Logger
	now       func() time.Time
	fireLimit int // tests cap the loop; 0 = unbounded
}

// SchedulerConfig carries the env-level knobs (EXPORT_SCHEDULE,
// EXPORT_TZ, EXPORT_LOCK_TTL, EXPORT_JOB_BUDGET) already parsed from the
// environment by services/worker/config.
type SchedulerConfig struct {
	Schedule string
	Timezone string
	LockTTL  time.Duration
	Budget   time.Duration
}

// NewScheduler validates the configuration fail-fast (maintainer decision
// 2026-10-27): a malformed schedule or zone, a non-positive TTL/budget, or
// a lock TTL that does not strictly exceed the job budget is an error,
// never a silently-overlapping scheduler — and binds the dependencies: the
// PO register on the worker's pool, the CSV exporter against the object
// store, and the tracker on KV + run store.
func NewScheduler(cfg SchedulerConfig, db *pgxpool.Pool, objects ObjectStore,
	kv KV, runs RunStore, log *slog.Logger) (*Scheduler, error) {
	if cfg.LockTTL <= 0 {
		return nil, fmt.Errorf("export: lock TTL must be positive, got %s", cfg.LockTTL)
	}
	if cfg.Budget <= 0 {
		return nil, fmt.Errorf("export: job budget must be positive, got %s", cfg.Budget)
	}
	if cfg.LockTTL <= cfg.Budget {
		return nil, fmt.Errorf("export: lock TTL %s must exceed job budget %s — otherwise the lock can expire mid-run and two schedulers overlap",
			cfg.LockTTL, cfg.Budget)
	}
	schedule, err := ParseSchedule(cfg.Schedule, cfg.Timezone)
	if err != nil {
		return nil, err
	}
	register := NewRegister(db)
	return &Scheduler{
		schedule: schedule,
		lockTTL:  cfg.LockTTL,
		budget:   cfg.Budget,
		tenants:  register,
		exporter: NewExporter(register, objects, schedule),
		tracker:  NewTracker(kv, runs, schedule, log),
		kv:       kv,
		log:      log,
		now:      time.Now,
	}, nil
}

// Run blocks until ctx is cancelled: a boot detection sweep, then the
// fire loop (sleep to the next slot, fire, repeat). Every tenant error is
// contained — a failed export is one tenant's failed run record, never a
// crash of the scheduler (PRD §14.8).
func (s *Scheduler) Run(ctx context.Context) error {
	s.bootDetect(ctx)
	for {
		now := s.now()
		next := s.schedule.Next(now)
		s.log.Info("export scheduler armed",
			"next_fire", next.UTC().Format(time.RFC3339),
			"tz", s.schedule.Location.String())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		s.fire(ctx, next)
		if s.fireLimit > 0 {
			s.fireLimit--
			if s.fireLimit == 0 {
				return nil
			}
		}
	}
}

// bootDetect sweeps the missed-run detection once per tenant so a miss is
// visible in the run history as soon as the worker is back, without
// waiting for the next fire (PRD §14.8 "Restart → missed-run detection
// alerts"). An unreadable tenant list only skips the sweep.
func (s *Scheduler) bootDetect(ctx context.Context) {
	tenants, err := s.tenants.Tenants(ctx)
	if err != nil {
		s.log.Warn("export boot detection skipped: cannot enumerate tenants", "error", err)
		return
	}
	for _, tenantID := range tenants {
		s.tracker.DetectAndFlag(ctx, tenantID, s.now())
	}
}

// fire runs every tenant's export for the slot at fireTime.
func (s *Scheduler) fire(ctx context.Context, fireTime time.Time) {
	tenants, err := s.tenants.Tenants(ctx)
	if err != nil {
		s.log.Error("export fire cannot enumerate tenants", "error", err)
		return
	}
	s.log.Info("export fire starting", "tenants", len(tenants),
		"slot", fireTime.UTC().Format(time.RFC3339))
	for _, tenantID := range tenants {
		s.runTenant(ctx, tenantID, fireTime)
	}
}

// runTenant acquires the per-tenant lock and hands the run to the winner.
// A Redis error or a held lock skips this tenant for this fire (log only);
// no run record is written — the record belongs to whichever instance won.
func (s *Scheduler) runTenant(ctx context.Context, tenantID string, fireTime time.Time) {
	token, acquired, err := s.kv.Acquire(ctx, LockKey(tenantID), s.lockTTL)
	if err != nil {
		s.log.Warn("export lock unavailable; tenant skipped this fire",
			"tenant_id", tenantID, "error", err)
		return
	}
	if !acquired {
		s.log.Info("export lock held elsewhere; tenant skipped this fire", "tenant_id", tenantID)
		return
	}
	defer func() {
		if err := s.kv.Release(ctx, LockKey(tenantID), token); err != nil {
			s.log.Warn("export lock release failed", "tenant_id", tenantID, "error", err)
		}
	}()
	s.runOwned(ctx, tenantID, fireTime)
}

// runOwned executes one tenant's export as the lock winner: detection,
// the export under the job budget, the run record (success or failed with
// its message), and the heartbeat on success. The record is written on a
// fresh context — the budget may be what expired the run, and a failed
// run must still be recorded (its message is #53's error display).
func (s *Scheduler) runOwned(ctx context.Context, tenantID string, fireTime time.Time) {
	started := s.now()
	runCtx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()

	missed, reason := s.tracker.Detect(runCtx, tenantID, fireTime)
	if missed {
		s.tracker.warnMissed(tenantID, reason, fireTime)
	}
	rowCount, objectKey, exportErr := s.exporter.Export(runCtx, tenantID, fireTime)
	finished := s.now()

	run := export.Run{
		RunID:          uuid.NewString(),
		TenantID:       tenantID,
		ScheduledFor:   fireTime,
		MissedPrevious: missed,
		StartedAt:      started,
		FinishedAt:     finished,
	}

	// WithoutCancel: SIGTERM cancels ctx mid-run (the export aborts with
	// ctx.Err) yet the failed record must still be written — symmetric with
	// the budget-exceeded case, where the budget, not the parent, expired
	// the run and the record lands anyway.
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), recordWriteBudget)
	defer recordCancel()

	if exportErr != nil {
		run.Status = export.StatusFailed
		run.ErrorMessage = exportErr.Error()
		s.log.Error("export run failed", "tenant_id", tenantID, "error", exportErr)
		if err := s.tracker.RecordFailure(recordCtx, run); err != nil {
			s.log.Error("export failed-run record write failed", "tenant_id", tenantID, "error", err)
		}
		return
	}

	run.Status = export.StatusSuccess
	run.RowCount = rowCount
	run.ObjectKey = objectKey
	s.log.Info("export run succeeded",
		"tenant_id", tenantID, "rows", rowCount, "object_key", objectKey)
	if err := s.tracker.RecordSuccess(recordCtx, tenantID, run); err != nil {
		s.log.Error("export run record write failed", "tenant_id", tenantID, "error", err)
	}
}
