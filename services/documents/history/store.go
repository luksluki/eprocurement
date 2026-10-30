package history

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Store persists the append-only history in MongoDB. Every caller-facing
// query is scoped by tenant_id; cross-tenant reads are indistinguishable
// from an empty history.
type Store struct {
	events *mongo.Collection
	audit  *mongo.Collection
}

// NewStore returns a store bound to the events and audit_log collections.
func NewStore(db *mongo.Database) *Store {
	return &Store{
		events: db.Collection(EventsCollectionName),
		audit:  db.Collection(AuditCollectionName),
	}
}

// EventIndexes and AuditIndexes pin the index set — name, keys, direction —
// for both collections (PRD §14.7). They are the single definition shared
// by EnsureIndexes here and scripts/seed, so the two can never drift. The
// unique event_id indexes are the consumer-dedupe mechanism (PRD §14.5):
// a replayed insert fails with a duplicate-key error that Apply treats as
// success. The tenant_occurred / tenant_entity_occurred indexes serve the
// ListEvents order (occurred_at DESC, event_id DESC) and the per-aggregate
// timeline filter.
func EventIndexes() []mongo.IndexModel {
	return []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "event_id", Value: 1}},
			Options: options.Index().SetName("uniq_event_id").SetUnique(true),
		},
		{
			Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "occurred_at", Value: -1}, {Key: "event_id", Value: -1}},
			Options: options.Index().SetName("tenant_occurred"),
		},
		{
			Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "entity_type", Value: 1},
				{Key: "entity_id", Value: 1}, {Key: "occurred_at", Value: -1}, {Key: "event_id", Value: -1}},
			Options: options.Index().SetName("tenant_entity_occurred"),
		},
	}
}

func AuditIndexes() []mongo.IndexModel {
	return []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "event_id", Value: 1}},
			Options: options.Index().SetName("uniq_event_id").SetUnique(true),
		},
		{
			Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "occurred_at", Value: -1}},
			Options: options.Index().SetName("tenant_occurred"),
		},
	}
}

// EnsureIndexes creates the pinned indexes (idempotent, safe on every boot).
// Against an already-indexed database it is a no-op (same names/keys), and
// against an empty one the builds scan nothing — the only state the service
// boots in. mongod 7.0.45 intermittently aborts with a WiredTiger invariant
// when an index build scans the million-event seeded collection, which is
// why scripts/seed creates the same index set before its bulk inserts
// instead of afterwards (dev-note 042).
func (s *Store) EnsureIndexes(ctx context.Context) error {
	if _, err := s.events.Indexes().CreateMany(ctx, EventIndexes()); err != nil {
		return fmt.Errorf("history: ensure events indexes: %w", err)
	}
	if _, err := s.audit.Indexes().CreateMany(ctx, AuditIndexes()); err != nil {
		return fmt.Errorf("history: ensure audit_log indexes: %w", err)
	}
	return nil
}

// EventBSON and AuditBSON return the stored document shapes. Both Apply and
// scripts/seed encode through them — one definition, no drift. The payload
// is stored as the raw JSON string (full jsonb fidelity: no key reordering,
// no number re-encoding), which is exactly what ListEvents returns.
func EventBSON(ev Event) bson.M {
	return bson.M{
		"event_id":    ev.EventID,
		"tenant_id":   ev.TenantID,
		"type":        ev.Type,
		"version":     ev.Version,
		"occurred_at": ev.OccurredAt,
		"actor_id":    ev.ActorID,
		"actor_ip":    ev.ActorIP,
		"trace_id":    ev.TraceID,
		"entity_type": ev.EntityType,
		"entity_id":   ev.EntityID,
		"payload":     ev.PayloadJSON,
	}
}

func AuditBSON(a AuditEntry) bson.M {
	return bson.M{
		"event_id":    a.EventID,
		"tenant_id":   a.TenantID,
		"actor_id":    a.ActorID,
		"actor_ip":    a.ActorIP,
		"action":      a.Action,
		"entity_type": a.EntityType,
		"entity_id":   a.EntityID,
		"details":     a.Details,
		"trace_id":    a.TraceID,
		"occurred_at": a.OccurredAt,
		"recorded_at": a.RecordedAt,
	}
}

// eventDoc decodes stored events. Fields may be absent in documents written
// before a field existed (or by hand) — decoding then yields the zero
// value instead of failing, pinned by TestApplyToleratesMissingFields.
type eventDoc struct {
	EventID     string    `bson:"event_id"`
	TenantID    string    `bson:"tenant_id"`
	Type        string    `bson:"type"`
	Version     int32     `bson:"version"`
	OccurredAt  time.Time `bson:"occurred_at"`
	ActorID     string    `bson:"actor_id"`
	ActorIP     string    `bson:"actor_ip"`
	TraceID     string    `bson:"trace_id"`
	EntityType  string    `bson:"entity_type"`
	EntityID    string    `bson:"entity_id"`
	PayloadJSON string    `bson:"payload"`
}

func (d eventDoc) toEvent() Event {
	return Event{
		EventID: d.EventID, TenantID: d.TenantID, Type: d.Type, Version: d.Version,
		OccurredAt: d.OccurredAt, ActorID: d.ActorID, ActorIP: d.ActorIP, TraceID: d.TraceID,
		EntityType: d.EntityType, EntityID: d.EntityID, PayloadJSON: d.PayloadJSON,
	}
}

// auditDoc decodes stored audit rows with the same missing-field tolerance
// as eventDoc (rows written before actor_ip existed decode to "").
type auditDoc struct {
	EventID    string    `bson:"event_id"`
	TenantID   string    `bson:"tenant_id"`
	ActorID    string    `bson:"actor_id"`
	ActorIP    string    `bson:"actor_ip"`
	Action     string    `bson:"action"`
	EntityType string    `bson:"entity_type"`
	EntityID   string    `bson:"entity_id"`
	Details    string    `bson:"details"`
	TraceID    string    `bson:"trace_id"`
	OccurredAt time.Time `bson:"occurred_at"`
	RecordedAt time.Time `bson:"recorded_at"`
}

func (d auditDoc) toAuditEntry() AuditEntry {
	return AuditEntry{
		EventID: d.EventID, TenantID: d.TenantID, ActorID: d.ActorID, ActorIP: d.ActorIP,
		Action: d.Action, EntityType: d.EntityType, EntityID: d.EntityID,
		Details: d.Details, TraceID: d.TraceID,
		OccurredAt: d.OccurredAt, RecordedAt: d.RecordedAt,
	}
}

// Apply projects one domain event: it appends the events document and, for
// decision event types, the audit_log row. Both writes dedupe on the unique
// event_id index — a duplicate-key error means the event was already
// applied and is treated as a successful replay, not an error. Apply is the
// idempotency boundary the worker consumers (#45–#47) rely on
// (PRD §14.5: "idempotency key = event_id").
//
// The two inserts are not wrapped in a transaction on purpose: the local
// MongoDB is a single-node mongod without a replica set, where
// multi-document transactions do not exist. Instead, each insert is
// individually idempotent on event_id, so a crash between the two leaves a
// partially applied event whose next Apply (or consumer retry) completes:
// the event insert dedupes, the audit insert lands. Partial recovery is
// pinned by TestApplyPartialRecovery.
//
// applied reports whether this call inserted the events document (false on
// replay). An error means the attempt failed and should be retried.
func (s *Store) Apply(ctx context.Context, ev Event) (applied bool, err error) {
	ev = ev.normalize()
	if err := ev.Validate(); err != nil {
		return false, err
	}
	_, err = s.events.InsertOne(ctx, EventBSON(ev))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			applied = false
		} else {
			return false, fmt.Errorf("history: append event: %w", err)
		}
	} else {
		applied = true
	}
	if action, decision := AuditAction(ev.Type); decision {
		entry := NewAuditEntry(ev, action, time.Now().UTC())
		if _, err := s.audit.InsertOne(ctx, AuditBSON(entry)); err != nil && !mongo.IsDuplicateKeyError(err) {
			return applied, fmt.Errorf("history: append audit: %w", err)
		}
	}
	return applied, nil
}

// ListFilter is the ListEvents query: an optional aggregate filter
// (entity_type + entity_id, both or neither) and a keyset cursor. Zero
// values fall back to the defaults; limit normalizes like the vendor list
// (below 1 → default, above max → max).
type ListFilter struct {
	EntityType string
	EntityID   string
	Limit      int
	Cursor     string // opaque cursor from a previous response
}

// Validate normalizes the filter: the entity fields are trimmed and must be
// both set or both empty, the limit is clamped, and the cursor — when
// present — must decode.
func (f ListFilter) Validate() (ListFilter, error) {
	f.EntityType = strings.TrimSpace(f.EntityType)
	f.EntityID = strings.TrimSpace(f.EntityID)
	if (f.EntityType == "") != (f.EntityID == "") {
		return f, ErrFilterInvalid
	}
	if f.Limit < 1 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if strings.TrimSpace(f.Cursor) != "" {
		if _, _, err := decodeCursor(f.Cursor); err != nil {
			return f, ErrCursorInvalid
		}
	}
	return f, nil
}

// List returns one page of the tenant's events, newest first — occurred_at
// DESC with event_id DESC as the tiebreaker, so decide events that share
// occurred_at (the approval engine writes step + terminal with one
// timestamp) get a total, stable order. Pagination is keyset-based: the
// cursor names the last (occurred_at, event_id) of the page and the next
// page continues strictly after it — pages never duplicate or skip rows,
// whatever is inserted in between. nextCursor is empty on the last page.
func (s *Store) List(ctx context.Context, tenantID string, f ListFilter) (events []Event, nextCursor string, err error) {
	f, err = f.Validate()
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(tenantID) == "" {
		return nil, "", ErrEventIncomplete
	}

	filter := bson.M{"tenant_id": tenantID}
	if f.EntityType != "" {
		filter["entity_type"] = f.EntityType
		filter["entity_id"] = f.EntityID
	}
	var afterOccurredAt time.Time
	var afterEventID string
	if f.Cursor != "" {
		afterOccurredAt, afterEventID, err = decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", ErrCursorInvalid
		}
		filter["$or"] = bson.A{
			bson.M{"occurred_at": bson.M{"$lt": afterOccurredAt}},
			bson.M{"occurred_at": afterOccurredAt, "event_id": bson.M{"$lt": afterEventID}},
		}
	}

	// Fetch one extra row to know whether another page exists.
	cursor, err := s.events.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "occurred_at", Value: -1}, {Key: "event_id", Value: -1}}).
		SetLimit(int64(f.Limit)+1))
	if err != nil {
		return nil, "", fmt.Errorf("history: list events: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var d eventDoc
		if err := cursor.Decode(&d); err != nil {
			return nil, "", fmt.Errorf("history: decode event: %w", err)
		}
		events = append(events, d.toEvent())
	}
	if err := cursor.Err(); err != nil {
		return nil, "", fmt.Errorf("history: list events: %w", err)
	}
	if len(events) > f.Limit {
		events = events[:f.Limit]
		last := events[f.Limit-1]
		nextCursor = encodeCursor(last.OccurredAt, last.EventID)
	}
	return events, nextCursor, nil
}

// The cursor is the page's last sort key — occurred_at as Unix milliseconds
// (exactly the precision MongoDB stores) plus the event_id tiebreaker —
// base64-encoded so the wire format stays opaque. Cursor payloads are
// validated as a unit: anything that does not decode is ErrCursorInvalid.
func encodeCursor(t time.Time, eventID string) string {
	raw := strconv.FormatInt(t.UnixMilli(), 10) + "|" + eventID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", ErrCursorInvalid
	}
	millis, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", err
	}
	if parts[1] == "" {
		return time.Time{}, "", ErrCursorInvalid
	}
	return time.UnixMilli(millis).UTC(), parts[1], nil
}

// AuditFilter is the ListAudit query (#44): an optional action filter, an
// optional aggregate filter (entity_type + entity_id, both or neither), and
// a keyset cursor — the ListFilter conventions on the audit collection.
type AuditFilter struct {
	Action     string
	EntityType string
	EntityID   string
	Limit      int
	Cursor     string // opaque cursor from a previous response
}

// Validate normalizes the filter: the entity fields are trimmed and must be
// both set or both empty, the action must be empty or one of AuditActions,
// the limit is clamped, and the cursor — when present — must decode.
func (f AuditFilter) Validate() (AuditFilter, error) {
	f.Action = strings.TrimSpace(f.Action)
	if f.Action != "" && !slices.Contains(AuditActions, f.Action) {
		return f, ErrActionInvalid
	}
	f.EntityType = strings.TrimSpace(f.EntityType)
	f.EntityID = strings.TrimSpace(f.EntityID)
	if (f.EntityType == "") != (f.EntityID == "") {
		return f, ErrFilterInvalid
	}
	if f.Limit < 1 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if strings.TrimSpace(f.Cursor) != "" {
		if _, _, err := decodeCursor(f.Cursor); err != nil {
			return f, ErrCursorInvalid
		}
	}
	return f, nil
}

// ListAudit returns one page of the tenant's audit rows, newest first —
// occurred_at DESC with event_id DESC as the tiebreaker (the List order;
// decision step + terminal pairs share occurred_at), keyset-paginated with
// the same cursor codec. Tenant scoping is absolute: another tenant's rows
// are invisible (empty result), the ListEvents convention.
func (s *Store) ListAudit(ctx context.Context, tenantID string, f AuditFilter) (entries []AuditEntry, nextCursor string, err error) {
	f, err = f.Validate()
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(tenantID) == "" {
		return nil, "", ErrEventIncomplete
	}

	filter := bson.M{"tenant_id": tenantID}
	if f.Action != "" {
		filter["action"] = f.Action
	}
	if f.EntityType != "" {
		filter["entity_type"] = f.EntityType
		filter["entity_id"] = f.EntityID
	}
	var afterOccurredAt time.Time
	var afterEventID string
	if f.Cursor != "" {
		afterOccurredAt, afterEventID, err = decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", ErrCursorInvalid
		}
		filter["$or"] = bson.A{
			bson.M{"occurred_at": bson.M{"$lt": afterOccurredAt}},
			bson.M{"occurred_at": afterOccurredAt, "event_id": bson.M{"$lt": afterEventID}},
		}
	}

	// Fetch one extra row to know whether another page exists.
	cursor, err := s.audit.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "occurred_at", Value: -1}, {Key: "event_id", Value: -1}}).
		SetLimit(int64(f.Limit)+1))
	if err != nil {
		return nil, "", fmt.Errorf("history: list audit: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var d auditDoc
		if err := cursor.Decode(&d); err != nil {
			return nil, "", fmt.Errorf("history: decode audit: %w", err)
		}
		entries = append(entries, d.toAuditEntry())
	}
	if err := cursor.Err(); err != nil {
		return nil, "", fmt.Errorf("history: list audit: %w", err)
	}
	if len(entries) > f.Limit {
		entries = entries[:f.Limit]
		last := entries[f.Limit-1]
		nextCursor = encodeCursor(last.OccurredAt, last.EventID)
	}
	return entries, nextCursor, nil
}

// ExportAudit reads the newest MaxAuditExportRows audit rows of the tenant
// for the CSV export (#44) — the row cap of the export's three-layer bound
// (bounded columns in AuditCSV, the gateway's raised gRPC receive limit,
// and this cap). Read-only, tenant-scoped like every caller-facing query.
func (s *Store) ExportAudit(ctx context.Context, tenantID string) ([]AuditEntry, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, ErrEventIncomplete
	}
	cursor, err := s.audit.Find(ctx, bson.M{"tenant_id": tenantID}, options.Find().
		SetSort(bson.D{{Key: "occurred_at", Value: -1}, {Key: "event_id", Value: -1}}).
		SetLimit(MaxAuditExportRows))
	if err != nil {
		return nil, fmt.Errorf("history: export audit: %w", err)
	}
	defer cursor.Close(ctx)
	entries := []AuditEntry{}
	for cursor.Next(ctx) {
		var d auditDoc
		if err := cursor.Decode(&d); err != nil {
			return nil, fmt.Errorf("history: decode audit: %w", err)
		}
		entries = append(entries, d.toAuditEntry())
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("history: export audit: %w", err)
	}
	return entries, nil
}
