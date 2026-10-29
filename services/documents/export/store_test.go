package export

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/luksluki/eprocurement/services/documents/internal/testenv"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db := testenv.Mongo(t)
	store := NewStore(db)
	if err := store.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	return store
}

func run(tenantID string, slot time.Time, status string) Run {
	return Run{
		RunID:        uuid.NewString(),
		TenantID:     tenantID,
		ScheduledFor: slot,
		Status:       status,
		ObjectKey:    "exports/" + tenantID + "/" + slot.Format("2006-01-02") + "/purchase-orders.csv",
		RowCount:     3,
		StartedAt:    slot.Add(time.Minute),
		FinishedAt:   slot.Add(2 * time.Minute),
	}
}

func day(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// TestUpsertRunOneRecordPerSlot pins the {tenant_id, scheduled_for}
// idempotency: re-running a slot overwrites the record (new run_id, new
// fields) instead of appending a second row — the manual re-run contract
// until #53 arrives.
func TestUpsertRunOneRecordPerSlot(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	slot := day("2026-10-26T01:00:00Z")

	first := run("t-49", slot, StatusFailed)
	first.ErrorMessage = "object store unreachable"
	first.RowCount = 0
	first.ObjectKey = ""
	if err := store.UpsertRun(ctx, first); err != nil {
		t.Fatalf("upsert first: %v", err)
	}

	second := run("t-49", slot, StatusSuccess)
	if err := store.UpsertRun(ctx, second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}

	runs, err := store.List(ctx, "t-49", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("records for one slot = %d, want 1", len(runs))
	}
	got := runs[0]
	if got.RunID != second.RunID || got.Status != StatusSuccess || got.ErrorMessage != "" {
		t.Fatalf("overwritten record = %+v, want the second (successful) run", got)
	}

	// A different slot is a different record.
	other := run("t-49", slot.Add(24*time.Hour), StatusSuccess)
	if err := store.UpsertRun(ctx, other); err != nil {
		t.Fatalf("upsert other slot: %v", err)
	}
	runs, err = store.List(ctx, "t-49", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 2 || !runs[0].ScheduledFor.Equal(other.ScheduledFor) {
		t.Fatalf("runs = %+v, want newest-first two slots", runs)
	}
}

// TestLatestAndHasSuccess covers the missed-run detection inputs: Latest
// separates tenants and reports the cold-start nil, and HasSuccess answers
// per half-open slot range (skewed timestamps still land in their slot).
func TestLatestAndHasSuccess(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	slot1 := day("2026-10-24T01:00:00Z")
	slot2 := day("2026-10-25T01:00:00Z")

	if latest, err := store.Latest(ctx, "t-cold"); err != nil || latest != nil {
		t.Fatalf("cold start Latest = %v, %v; want nil, nil", latest, err)
	}
	if ok, err := store.HasSuccess(ctx, "t-cold", slot1, slot2); err != nil || ok {
		t.Fatalf("cold start HasSuccess = %v, %v; want false, nil", ok, err)
	}

	success := run("t-49", slot1, StatusSuccess)
	if err := store.UpsertRun(ctx, success); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Same slot for another tenant: isolated.
	failed := run("t-other", slot1, StatusFailed)
	failed.ErrorMessage = "database unreachable"
	if err := store.UpsertRun(ctx, failed); err != nil {
		t.Fatalf("upsert other tenant: %v", err)
	}

	latest, err := store.Latest(ctx, "t-49")
	if err != nil || latest == nil || !latest.ScheduledFor.Equal(slot1) || latest.Status != StatusSuccess {
		t.Fatalf("Latest = %+v, %v", latest, err)
	}
	otherLatest, err := store.Latest(ctx, "t-other")
	if err != nil || otherLatest == nil || otherLatest.Status != StatusFailed {
		t.Fatalf("other tenant Latest = %+v, %v", otherLatest, err)
	}

	ok, err := store.HasSuccess(ctx, "t-49", slot1, slot2)
	if err != nil || !ok {
		t.Fatalf("HasSuccess(slot with success) = %v, %v; want true, nil", ok, err)
	}
	ok, err = store.HasSuccess(ctx, "t-49", slot2, slot2.Add(24*time.Hour))
	if err != nil || ok {
		t.Fatalf("HasSuccess(empty slot) = %v, %v; want false, nil", ok, err)
	}
	ok, err = store.HasSuccess(ctx, "t-other", slot1, slot2)
	if err != nil || ok {
		t.Fatalf("HasSuccess(failed run) = %v, %v; want false, nil", ok, err)
	}
}

// TestMarkNewestMissedPrevious pins the boot-time surfacing: the newest
// record is flagged in place, a flagged newest stays flagged, an empty
// tenant is a no-op, and other tenants stay untouched.
func TestMarkNewestMissedPrevious(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	slot1 := day("2026-10-24T01:00:00Z")
	slot2 := day("2026-10-25T01:00:00Z")

	if err := store.MarkNewestMissedPrevious(ctx, "t-empty"); err != nil {
		t.Fatalf("empty tenant mark: %v", err)
	}

	for _, s := range []time.Time{slot1, slot2} {
		if err := store.UpsertRun(ctx, run("t-49", s, StatusSuccess)); err != nil {
			t.Fatalf("upsert %s: %v", s, err)
		}
	}
	if err := store.UpsertRun(ctx, run("t-other", slot2, StatusSuccess)); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	if err := store.MarkNewestMissedPrevious(ctx, "t-49"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	latest, err := store.Latest(ctx, "t-49")
	if err != nil || latest == nil || !latest.MissedPrevious {
		t.Fatalf("newest not flagged: %+v, %v", latest, err)
	}
	older, err := store.List(ctx, "t-49", 10)
	if err != nil || len(older) != 2 || older[1].MissedPrevious {
		t.Fatalf("older records must stay unflagged: %+v, %v", older, err)
	}
	other, err := store.Latest(ctx, "t-other")
	if err != nil || other == nil || other.MissedPrevious {
		t.Fatalf("other tenant must stay unflagged: %+v, %v", other, err)
	}

	// Idempotent: re-marking keeps the single flag, no error.
	if err := store.MarkNewestMissedPrevious(ctx, "t-49"); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	runs, err := store.List(ctx, "t-49", 10)
	if err != nil || len(runs) != 2 || !runs[0].MissedPrevious {
		t.Fatalf("after re-mark: %+v, %v", runs, err)
	}
}

// TestGetTenantScopedAndValidated pins the download contract: ids are uuids
// (malformed → the typed invalid code), and a run id of another tenant is
// indistinguishable from a missing one.
func TestGetTenantScopedAndValidated(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	slot := day("2026-10-26T01:00:00Z")
	mine := run("t-49", slot, StatusSuccess)
	if err := store.UpsertRun(ctx, mine); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.UpsertRun(ctx, run("t-other", slot, StatusSuccess)); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	if _, err := store.Get(ctx, "t-49", "not-a-uuid"); err != ErrRunIDInvalid {
		t.Fatalf("malformed id err = %v, want ErrRunIDInvalid", err)
	}
	if _, err := store.Get(ctx, "t-49", ""); err != ErrRunIDInvalid {
		t.Fatalf("empty id err = %v, want ErrRunIDInvalid", err)
	}
	if _, err := store.Get(ctx, "t-49", uuid.NewString()); err != ErrRunNotFound {
		t.Fatalf("unknown id err = %v, want ErrRunNotFound", err)
	}
	otherRun, err := store.Get(ctx, "t-other", mine.RunID)
	if err != ErrRunNotFound || otherRun != nil {
		t.Fatalf("cross-tenant get = %v, %v; want ErrRunNotFound, nil", otherRun, err)
	}
	got, err := store.Get(ctx, "t-49", mine.RunID)
	if err != nil || got == nil || got.RunID != mine.RunID {
		t.Fatalf("own get = %+v, %v", got, err)
	}
}

// TestListLimitClamp mirrors the vendor-list convention: below 1 → default,
// above max → clamp.
func TestListLimitClamp(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	base := day("2026-10-01T01:00:00Z")
	for i := range 120 {
		if err := store.UpsertRun(ctx, run("t-49", base.Add(time.Duration(i)*24*time.Hour), StatusSuccess)); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	runs, err := store.List(ctx, "t-49", 0)
	if err != nil || len(runs) != DefaultLimit {
		t.Fatalf("limit 0 → %d runs, %v; want default %d", len(runs), err, DefaultLimit)
	}
	runs, err = store.List(ctx, "t-49", 500)
	if err != nil || len(runs) != MaxLimit {
		t.Fatalf("limit 500 → %d runs, %v; want max %d", len(runs), err, MaxLimit)
	}
	if _, err := store.List(ctx, " ", 10); err == nil {
		t.Fatal("empty tenant id must fail")
	}
}
