package notifications

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/luksluki/eprocurement/services/documents/history"
)

// Store persists notifications in MongoDB. The projection is append-only:
// documents are only ever inserted through Apply, never updated or deleted,
// and the package exports only EnsureIndexes, the document constructors, and
// Apply — the same surface discipline as history (PRD §17).
type Store struct {
	coll *mongo.Collection
}

// NewStore returns a store bound to the notifications collection.
func NewStore(db *mongo.Database) *Store {
	return &Store{coll: db.Collection(NotificationsCollectionName)}
}

// Indexes pins the index set — name, keys, direction (PRD §14.7). The
// unique event_id index is the consumer-dedupe mechanism (PRD §14.5
// "idempotency key = event_id"): a replayed insert fails with a
// duplicate-key error that Apply treats as success. The tenant_occurred
// compound mirrors history's tenant_occurred so a future read side pages a
// tenant's notifications newest-first.
func Indexes() []mongo.IndexModel {
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

// EnsureIndexes creates the pinned indexes (idempotent, safe on every boot;
// the worker calls it before consumption starts so a fresh database dedupes
// from the very first delivery).
func (s *Store) EnsureIndexes(ctx context.Context) error {
	if _, err := s.coll.Indexes().CreateMany(ctx, Indexes()); err != nil {
		return fmt.Errorf("notifications: ensure indexes: %w", err)
	}
	return nil
}

// NotificationBSON returns the stored document shape. Apply encodes through
// it — one definition, no drift (same convention as history.EventBSON).
func NotificationBSON(n Notification) bson.M {
	return bson.M{
		"event_id":     n.EventID,
		"tenant_id":    n.TenantID,
		"type":         n.Type,
		"version":      n.Version,
		"occurred_at":  n.OccurredAt,
		"actor_id":     n.ActorID,
		"trace_id":     n.TraceID,
		"entity_type":  n.EntityType,
		"entity_id":    n.EntityID,
		"recipient_id": n.RecipientID,
		"payload":      n.PayloadJSON,
		"recorded_at":  n.RecordedAt,
	}
}

// Apply projects one event into exactly one notification document. The
// insert dedupes on the unique event_id index — a duplicate-key error means
// the event was already applied and is a successful replay, not an error
// (PRD §14.5/§14.8). applied reports whether this call inserted the
// document (false on replay). An error means the attempt failed and should
// be retried; validation failures bubble the history sentinel errors
// (ErrEventIDInvalid / ErrEventIncomplete / ErrPayloadInvalid), which the
// worker consumer classifies as permanent and dead-letters without retry.
func (s *Store) Apply(ctx context.Context, ev history.Event) (applied bool, err error) {
	ev = normalize(ev)
	if err := ev.Validate(); err != nil {
		return false, fmt.Errorf("notifications: validate event: %w", err)
	}
	n := NewNotification(ev, time.Now().UTC())
	_, err = s.coll.InsertOne(ctx, NotificationBSON(n))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, fmt.Errorf("notifications: insert: %w", err)
	}
	return true, nil
}
