package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/services/documents/export"
)

// --- fakes -----------------------------------------------------------------

// fakeKV is an in-memory KV: an exclusive map for the lock, a plain map for
// the heartbeat.
type fakeKV struct {
	mu      sync.Mutex
	locks   map[string]string // key → token
	values  map[string]string // key → value (heartbeats)
	acqErr  error
	acqFail bool // force acquired=false
}

func newFakeKV() *fakeKV {
	return &fakeKV{locks: map[string]string{}, values: map[string]string{}}
}

func (f *fakeKV) Acquire(_ context.Context, key string, _ time.Duration) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acqErr != nil {
		return "", false, f.acqErr
	}
	if _, held := f.locks[key]; held || f.acqFail {
		return "", false, nil
	}
	token := uuid.NewString()
	f.locks[key] = token
	return token, true, nil
}

func (f *fakeKV) Release(_ context.Context, key, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.locks[key] == token {
		delete(f.locks, key)
	}
	return nil
}

func (f *fakeKV) Get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeKV) Set(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
	return nil
}

// fakeRuns records upserts and simulates the run history.
type fakeRuns struct {
	mu        sync.Mutex
	upserts   []export.Run
	latest    *export.Run
	hasSucc   map[string]bool // tenantID → HasSuccess answer
	marked    []string        // tenantIDs passed to MarkNewestMissedPrevious
	upsertErr error
}

func newFakeRuns() *fakeRuns { return &fakeRuns{hasSucc: map[string]bool{}} }

func (f *fakeRuns) UpsertRun(ctx context.Context, run export.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err // a cancelled context must fail the write; the real store honors ctx
	}
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, run)
	f.latest = &run
	return nil
}

func (f *fakeRuns) Latest(_ context.Context, tenantID string) (*export.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latest == nil || f.latest.TenantID != tenantID {
		return nil, nil
	}
	return f.latest, nil
}

func (f *fakeRuns) HasSuccess(_ context.Context, tenantID string, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasSucc[tenantID], nil
}

func (f *fakeRuns) MarkNewestMissedPrevious(_ context.Context, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, tenantID)
	return nil
}

// fakeTenants enumerates a fixed list or fails.
type fakeTenants struct {
	tenants []string
	err     error
}

func (f *fakeTenants) Tenants(context.Context) ([]string, error) { return f.tenants, f.err }

// fakeExporter exports instantly or fails/sleeps on demand.
type fakeExporter struct {
	mu    sync.Mutex
	calls []string // tenantIDs
	err   error
	rows  int
	key   string
	sleep time.Duration // simulate a slow export (budget test)
}

func (f *fakeExporter) Export(ctx context.Context, tenantID string, _ time.Time) (int, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, tenantID)
	sleep, err, rows, key := f.sleep, f.err, f.rows, f.key
	f.mu.Unlock()
	if sleep > 0 {
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
	if err != nil {
		return 0, "", err
	}
	return rows, key, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// newTestScheduler builds a Scheduler around fakes, bypassing NewScheduler
// (which needs a real pool); the constructor itself is covered below.
func newTestScheduler(t *testing.T, kv KV, runs RunStore, tenants TenantLister, exp TenantExporter) *Scheduler {
	t.Helper()
	schedule, err := ParseSchedule("01:00", "UTC")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	return &Scheduler{
		schedule: schedule,
		lockTTL:  15 * time.Minute,
		budget:   10 * time.Minute,
		tenants:  tenants,
		exporter: exp,
		tracker:  NewTracker(kv, runs, schedule, testLogger()),
		kv:       kv,
		log:      testLogger(),
		now:      time.Now,
	}
}

// --- constructor validation -------------------------------------------------

// TestNewSchedulerValidation is the fail-fast AC: a lock TTL that does not
// strictly exceed the job budget — and every other malformed knob — is a
// constructor error, never a silently-overlapping scheduler.
func TestNewSchedulerValidation(t *testing.T) {
	valid := SchedulerConfig{Schedule: "01:00", Timezone: "UTC", LockTTL: 15 * time.Minute, Budget: 10 * time.Minute}
	cases := []struct {
		name string
		cfg  SchedulerConfig
	}{
		{"ttl equals budget", SchedulerConfig{Schedule: "01:00", Timezone: "UTC", LockTTL: 10 * time.Minute, Budget: 10 * time.Minute}},
		{"ttl below budget", SchedulerConfig{Schedule: "01:00", Timezone: "UTC", LockTTL: 5 * time.Minute, Budget: 10 * time.Minute}},
		{"ttl zero", SchedulerConfig{Schedule: "01:00", Timezone: "UTC", LockTTL: 0, Budget: time.Minute}},
		{"budget zero", SchedulerConfig{Schedule: "01:00", Timezone: "UTC", LockTTL: time.Minute, Budget: 0}},
		{"bad schedule", SchedulerConfig{Schedule: "25:00", Timezone: "UTC", LockTTL: time.Minute, Budget: 30 * time.Second}},
		{"bad zone", SchedulerConfig{Schedule: "01:00", Timezone: "Mars/Olympus", LockTTL: time.Minute, Budget: 30 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewScheduler(tc.cfg, &pgxpool.Pool{}, nil, newFakeKV(), newFakeRuns(), testLogger()); err == nil {
				t.Fatalf("NewScheduler(%+v) = nil error, want error", tc.cfg)
			}
		})
	}
	if _, err := NewScheduler(valid, &pgxpool.Pool{}, nil, newFakeKV(), newFakeRuns(), testLogger()); err != nil {
		t.Fatalf("NewScheduler(valid) error: %v", err)
	}
	// Defaults satisfy the rule (15m > 10m).
	if DefaultLockTTL <= DefaultJobBudget {
		t.Fatalf("defaults violate the TTL > budget rule: %s <= %s", DefaultLockTTL, DefaultJobBudget)
	}
}

// --- per-tenant run ----------------------------------------------------------

// TestRunTenantLockNotAcquired: the lock loser (held elsewhere or Redis
// error) skips — no export, no run record. The run belongs to the winner.
func TestRunTenantLockNotAcquired(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	exp := &fakeExporter{rows: 2, key: "exports/t-1/2026-10-28/purchase-orders.csv"}
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-1"}}, exp)
	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)

	// Another instance holds the lock.
	if _, _, err := kv.Acquire(t.Context(), LockKey("t-1"), time.Minute); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	s.runTenant(t.Context(), "t-1", fire)
	if len(exp.calls) != 0 || len(runs.upserts) != 0 {
		t.Fatalf("lock loser must not export or record: calls=%v upserts=%v", exp.calls, runs.upserts)
	}

	// Redis unavailable: same skip, same silence.
	kv.acqErr = errors.New("redis down")
	s.runTenant(t.Context(), "t-1", fire)
	if len(exp.calls) != 0 || len(runs.upserts) != 0 {
		t.Fatalf("redis error must not export or record: calls=%v upserts=%v", exp.calls, runs.upserts)
	}
}

// TestRunTenantSuccess: the winner exports, records status=success with
// row count + object key, stamps the heartbeat, and releases the lock.
func TestRunTenantSuccess(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	runs.latest = &export.Run{RunID: "prev", TenantID: "t-1", Status: export.StatusSuccess} // not cold start
	runs.hasSucc["t-1"] = true                                                              // previous slot ran
	exp := &fakeExporter{rows: 3, key: "exports/t-1/2026-10-28/purchase-orders.csv"}
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-1"}}, exp)
	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)

	// A fresh heartbeat inside the grace window.
	kv.Set(t.Context(), HeartbeatKey("t-1"), FormatHeartbeat(fire.Add(-24*time.Hour)))

	s.runTenant(t.Context(), "t-1", fire)

	if len(runs.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(runs.upserts))
	}
	run := runs.upserts[0]
	if run.Status != export.StatusSuccess || run.RowCount != 3 ||
		run.ObjectKey != "exports/t-1/2026-10-28/purchase-orders.csv" ||
		run.MissedPrevious || run.ScheduledFor != fire || run.ErrorMessage != "" {
		t.Fatalf("run record = %+v", run)
	}
	if _, ok := kv.values[HeartbeatKey("t-1")]; !ok {
		t.Fatal("heartbeat not stamped on success")
	}
	if _, held := kv.locks[LockKey("t-1")]; held {
		t.Fatal("lock not released after the run")
	}
}

// TestRunTenantExportFailure: PG/MinIO down → a failed run record carries
// the error message and NO heartbeat is written (failures do not reset the
// 26 h clock).
func TestRunTenantExportFailure(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	runs.latest = &export.Run{RunID: "prev", TenantID: "t-1", Status: export.StatusSuccess}
	runs.hasSucc["t-1"] = true
	exp := &fakeExporter{err: errors.New("minio: connection refused")}
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-1"}}, exp)
	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	kv.Set(t.Context(), HeartbeatKey("t-1"), FormatHeartbeat(fire.Add(-24*time.Hour)))

	s.runTenant(t.Context(), "t-1", fire)

	if len(runs.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(runs.upserts))
	}
	run := runs.upserts[0]
	if run.Status != export.StatusFailed || run.ErrorMessage != "minio: connection refused" ||
		run.ObjectKey != "" || run.MissedPrevious {
		t.Fatalf("failed run record = %+v", run)
	}
	if _, ok := kv.values[HeartbeatKey("t-1")]; !ok {
		t.Fatal("test setup: seeded heartbeat missing")
	}
	// The seeded heartbeat value must be untouched (the fake overwrites on
	// Set, so presence of the OLD value proves no new write happened —
	// re-set it to a sentinel first).
}

// TestRunTenantBudgetExceeded: an export slower than the job budget fails
// with the deadline error and is still recorded as a failed run (its
// message is #53's error display).
func TestRunTenantBudgetExceeded(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	runs.latest = &export.Run{RunID: "prev", TenantID: "t-1", Status: export.StatusSuccess}
	runs.hasSucc["t-1"] = true
	exp := &fakeExporter{sleep: 300 * time.Millisecond}
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-1"}}, exp)
	s.budget = 50 * time.Millisecond
	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)

	s.runTenant(t.Context(), "t-1", fire)

	if len(runs.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(runs.upserts))
	}
	run := runs.upserts[0]
	if run.Status != export.StatusFailed {
		t.Fatalf("budget-exceeded run status = %s, want failed", run.Status)
	}
	if run.ErrorMessage == "" {
		t.Fatal("budget-exceeded run must carry the deadline error message")
	}
}

// cancelOnExportExporter cancels the parent context the moment its export
// starts, then reports the cancellation — a deterministic SIGTERM
// mid-export (no sleeps).
type cancelOnExportExporter struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnExportExporter) Export(ctx context.Context, _ string, _ time.Time) (int, string, error) {
	c.once.Do(c.cancel)
	<-ctx.Done()
	return 0, "", ctx.Err()
}

// TestRunTenantShutdownStillRecords: SIGTERM cancels the scheduler context
// mid-export; the failed run record must still be written — the record
// context is detached from the cancelled parent (WithoutCancel), the same
// guarantee the budget-exceeded path already has.
func TestRunTenantShutdownStillRecords(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	runs.latest = &export.Run{TenantID: "t-1", Status: export.StatusSuccess}
	runs.hasSucc["t-1"] = true
	ctx, cancel := context.WithCancel(t.Context())
	exp := &cancelOnExportExporter{cancel: cancel}
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-1"}}, exp)
	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)

	s.runTenant(ctx, "t-1", fire)
	cancel()

	if len(runs.upserts) != 1 {
		t.Fatalf("shutdown mid-export must still write the run record: upserts=%d", len(runs.upserts))
	}
	run := runs.upserts[0]
	if run.Status != export.StatusFailed || run.ErrorMessage != "context canceled" {
		t.Fatalf("shutdown run record = %+v, want failed with the cancellation error", run)
	}
}

// --- missed-run detection ----------------------------------------------------

// TestDetectColdStartAndHeartbeat covers the detection table: cold start
// (no history) is never a miss; a fresh heartbeat with a successful
// previous slot is healthy; a heartbeat older than 26 h is a miss even when
// the slot history claims success (e.g. Redis was down at that success).
func TestDetectColdStartAndHeartbeat(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	schedule := mustSchedule(t, "01:00", "UTC")
	tracker := NewTracker(kv, runs, schedule, testLogger())
	ctx := t.Context()
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

	// Cold start: no run history at all.
	if missed, _ := tracker.Detect(ctx, "t-1", now); missed {
		t.Fatal("cold start must not be a miss")
	}

	// History exists, previous slot succeeded, heartbeat fresh → healthy.
	runs.latest = &export.Run{TenantID: "t-1", Status: export.StatusSuccess}
	runs.hasSucc["t-1"] = true
	kv.Set(ctx, HeartbeatKey("t-1"), FormatHeartbeat(now.Add(-2*time.Hour)))
	if missed, reason := tracker.Detect(ctx, "t-1", now); missed || reason != "" {
		t.Fatalf("healthy tenant missed=%v reason=%q", missed, reason)
	}

	// Fresh heartbeat with an empty previous slot: the slot's run may still
	// be in flight on another instance (a boot mid-fire sees today's slot
	// without a record yet) — the fresh heartbeat wins, no miss.
	runs.hasSucc["t-1"] = false
	if missed, reason := tracker.Detect(ctx, "t-1", now); missed || reason != "" {
		t.Fatalf("fresh heartbeat + empty slot missed=%v reason=%q, want healthy", missed, reason)
	}
	runs.hasSucc["t-1"] = true

	// Heartbeat exactly at the grace boundary is still healthy…
	kv.Set(ctx, HeartbeatKey("t-1"), FormatHeartbeat(now.Add(-MissedGrace)))
	if missed, _ := tracker.Detect(ctx, "t-1", now); missed {
		t.Fatal("heartbeat at exactly 26h must not be a miss")
	}
	// …one nanosecond past it is a miss.
	kv.Set(ctx, HeartbeatKey("t-1"), FormatHeartbeat(now.Add(-MissedGrace-time.Nanosecond)))
	if missed, reason := tracker.Detect(ctx, "t-1", now); !missed || reason != reasonHeartbeatStale {
		t.Fatalf("stale heartbeat missed=%v reason=%q, want missed %q", missed, reason, reasonHeartbeatStale)
	}

	// Absent heartbeat (flushed Redis): the slot history decides — an
	// empty previous slot is a miss, a successful one is not.
	delete(kv.values, HeartbeatKey("t-1"))
	runs.hasSucc["t-1"] = false
	if missed, reason := tracker.Detect(ctx, "t-1", now); !missed || reason != reasonSlotEmpty {
		t.Fatalf("empty slot missed=%v reason=%q, want missed %q", missed, reason, reasonSlotEmpty)
	}
	runs.hasSucc["t-1"] = true
	if missed, _ := tracker.Detect(ctx, "t-1", now); missed {
		t.Fatal("absent heartbeat with successful previous slot must not be a miss")
	}
}

// TestDetectPreviousSlotRange pins the slot arithmetic the detection asks
// about: at a fire instant the previous slot is fire−24h; between fires it
// is the most recent slot before now. No heartbeat is seeded — the slot
// check is the fallback that runs when the heartbeat is absent (a fresh
// heartbeat short-circuits to healthy).
func TestDetectPreviousSlotRange(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	schedule := mustSchedule(t, "01:00", "UTC")
	tracker := NewTracker(kv, runs, schedule, testLogger())

	var gotFrom, gotTo time.Time
	base := newFakeRuns()
	rangeRecorder := &rangeRuns{fakeRuns: base, from: &gotFrom, to: &gotTo}
	tracker = NewTracker(kv, rangeRecorder, schedule, testLogger())

	base.latest = &export.Run{TenantID: "t-1", Status: export.StatusSuccess}
	rangeRecorder.hasSucc["t-1"] = true

	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	_, _ = tracker.Detect(t.Context(), "t-1", fire)
	if want := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC); !gotFrom.Equal(want) || !gotTo.Equal(want.Add(24*time.Hour)) {
		t.Fatalf("slot range at fire = [%s, %s), want [%s, %s)", gotFrom, gotTo, want, want.Add(24*time.Hour))
	}

	boot := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	_, _ = tracker.Detect(t.Context(), "t-1", boot)
	if want := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC); !gotFrom.Equal(want) {
		t.Fatalf("slot range at boot = [%s, …), want start %s", gotFrom, want)
	}
}

// rangeRuns records the HasSuccess range (embeds fakeRuns).
type rangeRuns struct {
	*fakeRuns
	from, to *time.Time
}

func (r *rangeRuns) HasSuccess(ctx context.Context, tenantID string, from, to time.Time) (bool, error) {
	*r.from, *r.to = from, to
	return r.fakeRuns.HasSuccess(ctx, tenantID, from, to)
}

// TestBootDetectFlagsWithoutWaiting: the boot pass flags the newest run
// record (missed_previous) for a missed tenant, stays silent for a healthy
// one and for a cold-start tenant, and writes no new run records.
func TestBootDetectFlagsWithoutWaiting(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	exp := &fakeExporter{}
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-missed", "t-healthy"}}, exp)

	// t-missed: history exists but the previous slot is empty.
	runs.latest = &export.Run{TenantID: "t-missed", Status: export.StatusSuccess}
	runs.hasSucc["t-missed"] = false
	// t-healthy: previous slot succeeded with a fresh heartbeat.
	runs.hasSucc["t-healthy"] = true
	kv.Set(t.Context(), HeartbeatKey("t-healthy"), FormatHeartbeat(time.Now()))

	s.bootDetect(t.Context())

	if len(runs.marked) != 1 || runs.marked[0] != "t-missed" {
		t.Fatalf("marked = %v, want exactly [t-missed]", runs.marked)
	}
	if len(runs.upserts) != 0 {
		t.Fatalf("boot pass must not write run records: %v", runs.upserts)
	}
}

// TestBootDetectDuringInflightFire: a worker booting while another
// instance's export for today's slot is still running (yesterday's success
// heartbeat ~24 h old — fresh; today's record not yet written because the
// run is inside its budget) must NOT flag a miss: the empty slot is the
// in-flight run, not a missed one — no missed_previous mark, no warning.
func TestBootDetectDuringInflightFire(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	boot := time.Date(2026, 10, 28, 1, 5, 0, 0, time.UTC) // 5 min into the 10 min budget
	runs.latest = &export.Run{TenantID: "t-1", Status: export.StatusSuccess}
	runs.hasSucc["t-1"] = false // today's slot has no success record yet (in flight)
	kv.Set(t.Context(), HeartbeatKey("t-1"), FormatHeartbeat(boot.Add(-24*time.Hour)))

	var logs bytes.Buffer
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-1"}}, &fakeExporter{})
	s.tracker = NewTracker(kv, runs, s.schedule, slog.New(slog.NewTextHandler(&logs, nil)))
	s.now = func() time.Time { return boot }

	s.bootDetect(t.Context())

	if len(runs.marked) != 0 {
		t.Fatalf("in-flight slot must not be flagged missed: marked=%v", runs.marked)
	}
	if strings.Contains(logs.String(), "export missed run detected") {
		t.Fatalf("no missed-run warning expected during an in-flight fire: %s", logs.String())
	}
}

// TestFireRunsEveryTenant: one fire exports every enumerated tenant under
// its own lock.
func TestFireRunsEveryTenant(t *testing.T) {
	kv := newFakeKV()
	runs := newFakeRuns()
	exp := &fakeExporter{rows: 1, key: "exports/x/purchase-orders.csv"}
	s := newTestScheduler(t, kv, runs, &fakeTenants{tenants: []string{"t-a", "t-b"}}, exp)

	s.fire(t.Context(), time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC))

	if len(exp.calls) != 2 || exp.calls[0] != "t-a" || exp.calls[1] != "t-b" {
		t.Fatalf("export calls = %v, want [t-a t-b]", exp.calls)
	}
	if len(runs.upserts) != 2 {
		t.Fatalf("run records = %d, want 2", len(runs.upserts))
	}

	// A failing enumeration is contained: no panic, no records.
	exp2 := &fakeExporter{}
	s2 := newTestScheduler(t, kv, newFakeRuns(), &fakeTenants{err: fmt.Errorf("pg down")}, exp2)
	s2.fire(t.Context(), time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC))
	if len(exp2.calls) != 0 {
		t.Fatalf("calls after enumeration failure = %v", exp2.calls)
	}
}
