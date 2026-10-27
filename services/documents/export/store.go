// Package export is the documents service's export run history (#49, FR-10):
// one MongoDB `export_runs` document per tenant per scheduled slot — the run
// record of the worker's nightly CSV export. Unlike events/audit_log this
// collection is NOT append-only: a slot's record is upserted (a re-run of the
// same date overwrites, keyed uniquely on {tenant_id, scheduled_for}), and
// the missed-run flag updates the newest record in place. The worker holds
// the write side through this store (the #47 notifications precedent: the
// worker projects, documents owns); the read side serves ListExportRuns /
// GetExportDownloadUrl. Every query is tenant-scoped; cross-tenant reads are
// indistinguishable from missing.
package export

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// RunsCollectionName is the export run history collection in the documents
// MongoDB database.
const RunsCollectionName = "export_runs"

// Run statuses.
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// Machine-readable failure codes — the gRPC message text at the API
// boundary, following the history package's convention.
var (
	ErrRunIDInvalid = errors.New("export_run_id_invalid")
	ErrRunNotFound  = errors.New("export_run_not_found")
)

// DefaultLimit and MaxLimit bound ListExportRuns pages (the vendor-list /
// ListEvents convention: below 1 → default, above max → clamp).
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Run is one export run record: what the worker did for one tenant in one
// scheduled slot. ObjectKey stays internal — it crosses no API surface
// (clients download through presigned URLs, the attachment convention).
type Run struct {
	RunID          string    // uuid, regenerated on overwrite
	TenantID       string    // tenant scoping
	ScheduledFor   time.Time // the slot this run belongs to (the fire time)
	Status         string    // success | failed
	ObjectKey      string    // exports/{tenant}/{yyyy-mm-dd}/purchase-orders.csv; empty on failure
	RowCount       int       // PO rows written to the CSV
	MissedPrevious bool      // detection found no successful run in the previous slot
	ErrorMessage   string    // failure detail; empty on success
	StartedAt      time.Time
	FinishedAt     time.Time
	UpdatedAt      time.Time
}

// Store persists export run records in MongoDB.
type Store struct {
	coll *mongo.Collection
}

// NewStore returns a store bound to the export_runs collection.
func NewStore(db *mongo.Database) *Store {
	return &Store{coll: db.Collection(RunsCollectionName)}
}

// Indexes pins the index set — name, keys, direction. uniq_tenant_slot is
// the idempotency mechanism: one record per tenant per scheduled slot, so a
// re-run of the same date upserts instead of appending a second row.
// tenant_slot serves the newest-first run history pages.
func Indexes() []mongo.IndexModel {
	return []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "tenant_id", Value: 1},
				{Key: "scheduled_for", Value: 1},
			},
			Options: options.Index().SetName("uniq_tenant_slot").SetUnique(true),
		},
		{
			Keys: bson.D{
				{Key: "tenant_id", Value: 1},
				{Key: "scheduled_for", Value: -1},
			},
			Options: options.Index().SetName("tenant_slot"),
		},
	}
}

// EnsureIndexes creates the pinned indexes (idempotent, safe on every boot).
// The worker calls it before the scheduler starts so a fresh database
// dedupes from the very first run.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	if _, err := s.coll.Indexes().CreateMany(ctx, Indexes()); err != nil {
		return fmt.Errorf("export: ensure indexes: %w", err)
	}
	return nil
}

// RunBSON returns the stored document shape — one definition, no drift (the
// history/notifications convention).
func RunBSON(r Run) bson.M {
	return bson.M{
		"run_id":          r.RunID,
		"tenant_id":       r.TenantID,
		"scheduled_for":   r.ScheduledFor,
		"status":          r.Status,
		"object_key":      r.ObjectKey,
		"row_count":       r.RowCount,
		"missed_previous": r.MissedPrevious,
		"error_message":   r.ErrorMessage,
		"started_at":      r.StartedAt,
		"finished_at":     r.FinishedAt,
		"updated_at":      r.UpdatedAt,
	}
}

// UpsertRun writes one run record keyed on {tenant_id, scheduled_for}: the
// first write inserts, a re-run of the same slot overwrites (the worker
// regenerates run_id). The record keeps whatever fields r carries — the
// caller assembles the full record; there are no partial merges.
func (s *Store) UpsertRun(ctx context.Context, r Run) error {
	if r.TenantID == "" {
		return fmt.Errorf("export: upsert run: empty tenant id")
	}
	if r.RunID == "" {
		return fmt.Errorf("export: upsert run: empty run id")
	}
	r.UpdatedAt = time.Now().UTC()
	_, err := s.coll.UpdateOne(ctx,
		bson.M{"tenant_id": r.TenantID, "scheduled_for": r.ScheduledFor},
		bson.M{"$set": RunBSON(r)},
		options.Update().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("export: upsert run: %w", err)
	}
	return nil
}

// runDoc decodes stored run records. Fields may be absent in documents
// written before a field existed — decoding then yields the zero value
// (the history eventDoc convention).
type runDoc struct {
	RunID          string    `bson:"run_id"`
	TenantID       string    `bson:"tenant_id"`
	ScheduledFor   time.Time `bson:"scheduled_for"`
	Status         string    `bson:"status"`
	ObjectKey      string    `bson:"object_key"`
	RowCount       int       `bson:"row_count"`
	MissedPrevious bool      `bson:"missed_previous"`
	ErrorMessage   string    `bson:"error_message"`
	StartedAt      time.Time `bson:"started_at"`
	FinishedAt     time.Time `bson:"finished_at"`
	UpdatedAt      time.Time `bson:"updated_at"`
}

func (d runDoc) toRun() Run {
	return Run{
		RunID: d.RunID, TenantID: d.TenantID, ScheduledFor: d.ScheduledFor,
		Status: d.Status, ObjectKey: d.ObjectKey, RowCount: d.RowCount,
		MissedPrevious: d.MissedPrevious, ErrorMessage: d.ErrorMessage,
		StartedAt: d.StartedAt, FinishedAt: d.FinishedAt, UpdatedAt: d.UpdatedAt,
	}
}

// Latest returns the tenant's newest run record by scheduled_for, or nil
// when the tenant has none (the cold-start signal for missed-run
// detection).
func (s *Store) Latest(ctx context.Context, tenantID string) (*Run, error) {
	cursor, err := s.coll.Find(ctx, bson.M{"tenant_id": tenantID},
		options.Find().SetSort(bson.D{{Key: "scheduled_for", Value: -1}}).SetLimit(1))
	if err != nil {
		return nil, fmt.Errorf("export: latest run: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var d runDoc
		if err := cursor.Decode(&d); err != nil {
			return nil, fmt.Errorf("export: decode run: %w", err)
		}
		run := d.toRun()
		return &run, nil
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("export: latest run: %w", err)
	}
	return nil, nil
}

// HasSuccess reports whether the tenant has a successful run whose slot
// falls in [from, to). The missed-run detection asks it about the previous
// slot; the half-open range tolerates clock skew that exact-equality
// matching on scheduled_for would not.
func (s *Store) HasSuccess(ctx context.Context, tenantID string, from, to time.Time) (bool, error) {
	count, err := s.coll.CountDocuments(ctx, bson.M{
		"tenant_id":     tenantID,
		"status":        StatusSuccess,
		"scheduled_for": bson.M{"$gte": from, "$lt": to},
	}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("export: has success: %w", err)
	}
	return count > 0, nil
}

// MarkNewestMissedPrevious flags the tenant's newest run record with
// missed_previous=true — the boot-time pass of the missed-run detection
// (#49: a miss surfaces in the run history without waiting for the next
// fire). Idempotent: an already-flagged newest record stays flagged, and a
// tenant with no records is a no-op (cold start).
func (s *Store) MarkNewestMissedPrevious(ctx context.Context, tenantID string) error {
	latest, err := s.Latest(ctx, tenantID)
	if err != nil {
		return err
	}
	if latest == nil || latest.MissedPrevious {
		return nil
	}
	_, err = s.coll.UpdateOne(ctx,
		bson.M{"tenant_id": tenantID, "scheduled_for": latest.ScheduledFor},
		bson.M{"$set": bson.M{"missed_previous": true, "updated_at": time.Now().UTC()}})
	if err != nil {
		return fmt.Errorf("export: mark missed: %w", err)
	}
	return nil
}

// List returns one page of the tenant's run history, newest first
// (scheduled_for DESC, run_id DESC as the tiebreaker — two runs of one slot
// share the timestamp only in a mid-overwrite window). limit normalizes
// like the vendor list (below 1 → default, above max → clamp).
func (s *Store) List(ctx context.Context, tenantID string, limit int) ([]Run, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("export: list runs: empty tenant id")
	}
	if limit < 1 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	cursor, err := s.coll.Find(ctx, bson.M{"tenant_id": tenantID},
		options.Find().
			SetSort(bson.D{{Key: "scheduled_for", Value: -1}, {Key: "run_id", Value: -1}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("export: list runs: %w", err)
	}
	defer cursor.Close(ctx)
	runs := []Run{}
	for cursor.Next(ctx) {
		var d runDoc
		if err := cursor.Decode(&d); err != nil {
			return nil, fmt.Errorf("export: decode run: %w", err)
		}
		runs = append(runs, d.toRun())
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("export: list runs: %w", err)
	}
	return runs, nil
}

// Get returns one run by id, scoped to the tenant: another tenant's run id
// is indistinguishable from a missing one (ErrRunNotFound).
func (s *Store) Get(ctx context.Context, tenantID, runID string) (*Run, error) {
	trimmed := strings.TrimSpace(runID)
	if trimmed == "" {
		return nil, ErrRunIDInvalid
	}
	if _, err := uuid.Parse(trimmed); err != nil {
		return nil, ErrRunIDInvalid
	}
	res := s.coll.FindOne(ctx, bson.M{"tenant_id": tenantID, "run_id": trimmed})
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("export: get run: %w", err)
	}
	var d runDoc
	if err := res.Decode(&d); err != nil {
		return nil, fmt.Errorf("export: decode run: %w", err)
	}
	run := d.toRun()
	return &run, nil
}
