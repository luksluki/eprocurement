package export

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/services/documents/export"
	"github.com/luksluki/eprocurement/services/documents/history"
)

// fakeManualRegister records the window it was asked for and answers with
// rows, an error, or a sleep (budget tests).
type fakeManualRegister struct {
	mu      chan struct{} // nil unless a sleep is wanted
	rows    []PurchaseOrderRow
	err     error
	sleep   time.Duration
	gotFrom map[string]time.Time
	gotTo   map[string]time.Time
	calls   int
}

func (f *fakeManualRegister) PurchaseOrders(ctx context.Context, tenantID string, from, to time.Time) ([]PurchaseOrderRow, error) {
	f.calls++
	if f.gotFrom == nil {
		f.gotFrom, f.gotTo = map[string]time.Time{}, map[string]time.Time{}
	}
	f.gotFrom[tenantID], f.gotTo[tenantID] = from, to
	if f.sleep > 0 {
		select {
		case <-time.After(f.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakeManualObjects records every Put.
type fakeManualObjects struct {
	keys  []string
	blobs map[string]string
	err   error
}

func (f *fakeManualObjects) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	if f.err != nil {
		return f.err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.blobs == nil {
		f.blobs = map[string]string{}
	}
	f.keys = append(f.keys, key)
	f.blobs[key] = string(body)
	return nil
}

func manualEvent(t *testing.T, occurredAt time.Time) history.Event {
	t.Helper()
	ev, err := history.NewEvent(
		"ev-53", "t-53", EventRequestedType, occurredAt,
		"user-53", "203.0.113.7", "trace-53", "export", "ev-53",
		`{"requested_by":"user-53"}`)
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	return ev
}

func newTestManualRunner(t *testing.T, cfg ManualConfig, register ManualRegister, objects ObjectStore, runs RunStore) *ManualRunner {
	t.Helper()
	h, err := NewManualRunner(cfg, register, objects, runs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManualRunner: %v", err)
	}
	return h
}

// TestManualObjectKey pins the manual key: the -manual-{HHMMSS} twin of
// the nightly key, with date AND clock from the anchor in EXPORT_TZ.
func TestManualObjectKey(t *testing.T) {
	utc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatalf("utc: %v", err)
	}
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("jakarta: %v", err)
	}
	anchor := time.Date(2026, 11, 2, 18, 30, 45, 0, time.UTC) // 01:30:45 Jakarta on Nov 3

	if got, want := ManualObjectKey("t-1", utc, anchor),
		"exports/t-1/2026-11-02/purchase-orders-manual-183045.csv"; got != want {
		t.Fatalf("utc key = %s, want %s", got, want)
	}
	if got, want := ManualObjectKey("t-1", jakarta, anchor),
		"exports/t-1/2026-11-03/purchase-orders-manual-013045.csv"; got != want {
		t.Fatalf("jakarta key = %s, want %s", got, want)
	}
	// Two triggers a minute apart never collide; only same-second ones do
	// (the documented known edge).
	a := ManualObjectKey("t-1", utc, anchor)
	b := ManualObjectKey("t-1", utc, anchor.Add(61*time.Second))
	if a == b {
		t.Fatalf("keys of runs a minute apart collide: %s", a)
	}
}

// TestNewManualRunnerValidation: a non-positive budget or a malformed zone
// is a constructor error (the NewScheduler convention — fail fast at boot).
func TestNewManualRunnerValidation(t *testing.T) {
	for name, cfg := range map[string]ManualConfig{
		"zero budget":     {Timezone: "UTC", Budget: 0},
		"negative budget": {Timezone: "UTC", Budget: -time.Minute},
		"bad zone":        {Timezone: "Mars/Olympus", Budget: time.Minute},
	} {
		if _, err := NewManualRunner(cfg, &fakeManualRegister{}, &fakeManualObjects{}, newFakeRuns(), nil); err == nil {
			t.Fatalf("%s: want constructor error", name)
		}
	}
	if _, err := NewManualRunner(ManualConfig{Timezone: "Asia/Jakarta", Budget: time.Minute},
		&fakeManualRegister{}, &fakeManualObjects{}, newFakeRuns(), nil); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

// TestManualRunnerSuccess: the happy path — the window is the trailing
// 24 h before the event's occurred_at (deterministic across retries), the
// CSV lands under the manual key, and the record carries trigger=manual
// with scheduled_for = occurred_at.
func TestManualRunnerSuccess(t *testing.T) {
	occurredAt := time.Date(2026, 11, 2, 9, 30, 12, 0, time.UTC)
	register := &fakeManualRegister{rows: []PurchaseOrderRow{{
		PONumber: "PO-000001", VendorCode: "V-1", VendorName: "PT One",
		Currency: "IDR", TotalCents: 15450000, Status: "issued",
		IssuedAt: occurredAt.Add(-2 * time.Hour),
	}}}
	objects := &fakeManualObjects{}
	runs := newFakeRuns()
	h := newTestManualRunner(t, ManualConfig{Timezone: "UTC", Budget: time.Minute}, register, objects, runs)

	applied, err := h.Apply(t.Context(), manualEvent(t, occurredAt))
	if err != nil || !applied {
		t.Fatalf("apply = %v/%v, want true/nil", applied, err)
	}
	if from, to := register.gotFrom["t-53"], register.gotTo["t-53"]; !from.Equal(occurredAt.Add(-24*time.Hour)) || !to.Equal(occurredAt) {
		t.Fatalf("window = [%s, %s), want the trailing 24 h before occurred_at", from, to)
	}
	wantKey := "exports/t-53/2026-11-02/purchase-orders-manual-093012.csv"
	if len(objects.keys) != 1 || objects.keys[0] != wantKey {
		t.Fatalf("object keys = %v, want [%s]", objects.keys, wantKey)
	}
	if blob := objects.blobs[wantKey]; !strings.Contains(blob, "PO-000001") || !strings.HasPrefix(blob, "po_number,") {
		t.Fatalf("stored csv = %q, want the pinned header + the row", blob)
	}
	if len(runs.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(runs.upserts))
	}
	run := runs.upserts[0]
	if run.Trigger != export.TriggerManual || run.Status != export.StatusSuccess ||
		run.ScheduledFor != occurredAt || run.RowCount != 1 || run.ObjectKey != wantKey ||
		run.MissedPrevious || run.ErrorMessage != "" {
		t.Fatalf("run record = %+v", run)
	}
}

// TestManualRunnerRetryOverwritesSameSlot: a broker redelivery of the same
// event re-exports the same window (anchored on occurred_at) and upserts
// the SAME {tenant, scheduled_for} record — the retry idempotency.
func TestManualRunnerRetryOverwritesSameSlot(t *testing.T) {
	occurredAt := time.Date(2026, 11, 2, 9, 30, 12, 0, time.UTC)
	register := &fakeManualRegister{rows: nil}
	objects := &fakeManualObjects{}
	runs := newFakeRuns()
	h := newTestManualRunner(t, ManualConfig{Timezone: "UTC", Budget: time.Minute}, register, objects, runs)
	ev := manualEvent(t, occurredAt)

	for i := 0; i < 2; i++ {
		if _, err := h.Apply(t.Context(), ev); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if register.calls != 2 {
		t.Fatalf("register calls = %d, want 2 (redelivery re-exports)", register.calls)
	}
	if len(runs.upserts) != 2 || runs.upserts[0].ScheduledFor != runs.upserts[1].ScheduledFor {
		t.Fatalf("upserts = %+v, want two records on the same upsert key", runs.upserts)
	}
}

// TestManualRunnerFailureRecordsAndReturns: a register failure writes the
// failed record (trigger=manual, the error message) AND returns the error
// so the consumer's retry ladder (5 s/30 s/5 m → DLQ) applies.
func TestManualRunnerFailureRecordsAndReturns(t *testing.T) {
	occurredAt := time.Date(2026, 11, 2, 9, 30, 12, 0, time.UTC)
	register := &fakeManualRegister{err: errors.New("pg: connection refused")}
	objects := &fakeManualObjects{}
	runs := newFakeRuns()
	h := newTestManualRunner(t, ManualConfig{Timezone: "UTC", Budget: time.Minute}, register, objects, runs)

	applied, err := h.Apply(t.Context(), manualEvent(t, occurredAt))
	if err == nil || applied {
		t.Fatalf("apply = %v/%v, want false/<error> (retry ladder must see it)", applied, err)
	}
	if len(runs.upserts) != 1 {
		t.Fatalf("upserts = %d, want the failed record", len(runs.upserts))
	}
	run := runs.upserts[0]
	if run.Status != export.StatusFailed || run.Trigger != export.TriggerManual ||
		!strings.Contains(run.ErrorMessage, "pg: connection refused") || run.ObjectKey != "" {
		t.Fatalf("failed record = %+v", run)
	}
	if len(objects.keys) != 0 {
		t.Fatalf("objects written = %v, want none", objects.keys)
	}
}

// TestManualRunnerBudgetExceededStillRecords: an export slower than the
// job budget fails with the deadline, and the failed run record is still
// written on the fresh context (the runOwned pattern).
func TestManualRunnerBudgetExceededStillRecords(t *testing.T) {
	occurredAt := time.Date(2026, 11, 2, 9, 30, 12, 0, time.UTC)
	register := &fakeManualRegister{sleep: 300 * time.Millisecond}
	objects := &fakeManualObjects{}
	runs := newFakeRuns()
	h := newTestManualRunner(t, ManualConfig{Timezone: "UTC", Budget: 50 * time.Millisecond}, register, objects, runs)

	applied, err := h.Apply(t.Context(), manualEvent(t, occurredAt))
	if err == nil || applied {
		t.Fatalf("apply = %v/%v, want false/<deadline>", applied, err)
	}
	if len(runs.upserts) != 1 || runs.upserts[0].Status != export.StatusFailed ||
		!errors.Is(err, context.DeadlineExceeded) && !strings.Contains(runs.upserts[0].ErrorMessage, "context deadline exceeded") {
		t.Fatalf("upserts = %+v, err = %v — the budget-exceeded record must land", runs.upserts, err)
	}
}

// TestManualRunnerRecordSurvivesParentCancel: SIGTERM (parent ctx
// cancelled) mid-export still lands the failed record — the record context
// is WithoutCancel.
func TestManualRunnerRecordSurvivesParentCancel(t *testing.T) {
	occurredAt := time.Date(2026, 11, 2, 9, 30, 12, 0, time.UTC)
	register := &fakeManualRegister{sleep: 10 * time.Millisecond}
	objects := &fakeManualObjects{}
	runs := newFakeRuns()
	h := newTestManualRunner(t, ManualConfig{Timezone: "UTC", Budget: time.Minute}, register, objects, runs)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	_, err := h.Apply(ctx, manualEvent(t, occurredAt))
	if err == nil {
		t.Fatal("cancelled parent must fail the export")
	}
	if len(runs.upserts) != 1 || runs.upserts[0].Status != export.StatusFailed {
		t.Fatalf("upserts = %+v — the record must survive the parent cancel", runs.upserts)
	}
}

// TestManualRunnerWrongType: an event that is not export.requested is an
// error (topology drift — it climbs the ladder to a visible DLQ copy), and
// no export work runs.
func TestManualRunnerWrongType(t *testing.T) {
	register := &fakeManualRegister{}
	objects := &fakeManualObjects{}
	runs := newFakeRuns()
	h := newTestManualRunner(t, ManualConfig{Timezone: "UTC", Budget: time.Minute}, register, objects, runs)

	ev, err := history.NewEvent("ev-x", "t-53", "pr.submitted",
		time.Date(2026, 11, 2, 9, 30, 12, 0, time.UTC),
		"user-53", "", "", "requisition", "req-1", `{}`)
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	if _, err := h.Apply(t.Context(), ev); err == nil || !strings.Contains(err.Error(), "pr.submitted") {
		t.Fatalf("err = %v, want it to name the stray type", err)
	}
	if register.calls != 0 || len(runs.upserts) != 0 {
		t.Fatalf("register calls = %d, upserts = %d — no work must run", register.calls, len(runs.upserts))
	}
}

// TestManualDeliveryTimeout pins the runner sizing: budget + record budget
// + margin, strictly above the budget, and — with the DEFAULT job budget —
// far above the #47 runner's 10 s cap, which is exactly why the exports
// queue needs its own runner.
func TestManualDeliveryTimeout(t *testing.T) {
	got, err := ManualDeliveryTimeout(30 * time.Second)
	if err != nil {
		t.Fatalf("30s: %v", err)
	}
	if want := 30*time.Second + RecordWriteBudget + manualTimeoutMargin; got != want {
		t.Fatalf("timeout = %s, want %s", got, want)
	}
	if _, err := ManualDeliveryTimeout(0); err == nil {
		t.Fatal("zero budget: want error")
	}
	def, err := ManualDeliveryTimeout(DefaultJobBudget)
	if err != nil {
		t.Fatalf("default budget: %v", err)
	}
	if def <= DefaultJobBudget {
		t.Fatalf("timeout %s must strictly exceed the job budget %s", def, DefaultJobBudget)
	}
	if def <= 10*time.Second {
		t.Fatalf("timeout %s must exceed the #47 runner default cap of 10s", def)
	}
}

// TestManualRunnerSucceedsUnderRunnerCap closes the small-demo gap: under
// the WIRED runner (DeliveryTimeout = ManualDeliveryTimeout(budget), the
// consumer's WithTimeout around Apply), an export slower than the #47
// default cap still succeeds when it fits its own budget; under a tight
// runner cap smaller than the work, it fails. The cap, not the budget, is
// what cancels first when mis-wired.
func TestManualRunnerSucceedsUnderRunnerCap(t *testing.T) {
	occurredAt := time.Date(2026, 11, 2, 9, 30, 12, 0, time.UTC)
	const budget = 300 * time.Millisecond
	const work = 150 * time.Millisecond // > the #47 default cap's fraction; fits the budget

	cases := []struct {
		name    string
		cap     time.Duration
		wantErr bool
	}{
		{"wired cap (budget+record+margin)", budget + RecordWriteBudget + manualTimeoutMargin, false},
		{"tight cap below the work", 50 * time.Millisecond, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			register := &fakeManualRegister{sleep: work}
			objects := &fakeManualObjects{}
			runs := newFakeRuns()
			h := newTestManualRunner(t, ManualConfig{Timezone: "UTC", Budget: budget}, register, objects, runs)

			// The consumer's exact wrapping: handle runs under
			// context.WithTimeout(ctx, DeliveryTimeout).
			runnerCtx, cancel := context.WithTimeout(t.Context(), tc.cap)
			defer cancel()
			_, err := h.Apply(runnerCtx, manualEvent(t, occurredAt))
			if tc.wantErr && err == nil {
				t.Fatal("tight cap: want the run to fail")
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("wired cap: %v", err)
				}
				if len(objects.keys) != 1 || len(runs.upserts) != 1 ||
					runs.upserts[0].Status != export.StatusSuccess {
					t.Fatalf("objects = %v, upserts = %+v — the slow-but-in-budget run must succeed", objects.keys, runs.upserts)
				}
			}
		})
	}
}
