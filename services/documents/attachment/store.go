package attachment

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CollectionName is the PRD §13 metadata collection in MongoDB.
const CollectionName = "document_meta"

// Store persists attachment metadata in MongoDB document_meta. Every
// caller-facing query is scoped by tenant_id (cross-tenant reads are
// indistinguishable from missing); only the reaper sweeps across tenants.
type Store struct {
	coll *mongo.Collection
}

// NewStore returns a store bound to the document_meta collection.
func NewStore(db *mongo.Database) *Store {
	return &Store{coll: db.Collection(CollectionName)}
}

// mongoDoc mirrors Attachment; UploadedAt is null while pending.
type mongoDoc struct {
	ID             string     `bson:"_id"`
	TenantID       string     `bson:"tenant_id"`
	EntityType     string     `bson:"entity_type"`
	EntityID       string     `bson:"entity_id"`
	Filename       string     `bson:"filename"`
	MimeType       string     `bson:"mime_type"`
	SizeBytes      int64      `bson:"size_bytes"`
	ChecksumSHA256 string     `bson:"checksum_sha256"`
	Status         string     `bson:"status"`
	ObjectKey      string     `bson:"object_key"`
	CreatedBy      string     `bson:"created_by"`
	CreatedAt      time.Time  `bson:"created_at"`
	UploadedAt     *time.Time `bson:"uploaded_at,omitempty"`
}

// EnsureIndexes creates the lookup indexes (idempotent, safe on every boot).
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "entity_type", Value: 1}, {Key: "entity_id", Value: 1}, {Key: "created_at", Value: 1}},
			Options: options.Index().SetName("owner_lookup"),
		},
		{
			Keys:    bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: 1}},
			Options: options.Index().SetName("reaper_pending"),
		},
	})
	return err
}

func (d mongoDoc) toAttachment() Attachment {
	a := Attachment{
		ID: d.ID, TenantID: d.TenantID, EntityType: d.EntityType, EntityID: d.EntityID,
		Filename: d.Filename, MimeType: d.MimeType, SizeBytes: d.SizeBytes,
		ChecksumSHA256: d.ChecksumSHA256, Status: d.Status, ObjectKey: d.ObjectKey,
		CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt,
	}
	if d.UploadedAt != nil {
		a.UploadedAt = *d.UploadedAt
	}
	return a
}

func fromAttachment(a Attachment) mongoDoc {
	d := mongoDoc{
		ID: a.ID, TenantID: a.TenantID, EntityType: a.EntityType, EntityID: a.EntityID,
		Filename: a.Filename, MimeType: a.MimeType, SizeBytes: a.SizeBytes,
		ChecksumSHA256: a.ChecksumSHA256, Status: a.Status, ObjectKey: a.ObjectKey,
		CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt,
	}
	if !a.UploadedAt.IsZero() {
		d.UploadedAt = &a.UploadedAt
	}
	return d
}

// Insert stores new pending metadata.
func (s *Store) Insert(ctx context.Context, a Attachment) error {
	_, err := s.coll.InsertOne(ctx, fromAttachment(a))
	return err
}

// Get returns one attachment of the caller's tenant.
func (s *Store) Get(ctx context.Context, tenantID, id string) (Attachment, error) {
	if !ValidID(id) {
		return Attachment{}, ErrIDInvalid
	}
	var d mongoDoc
	err := s.coll.FindOne(ctx, bson.M{"_id": id, "tenant_id": tenantID}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	return d.toAttachment(), nil
}

// List returns the attachments of one owner, oldest first, pending and
// ready alike.
func (s *Store) List(ctx context.Context, tenantID, entityType, entityID string) ([]Attachment, error) {
	if !ValidEntity(entityType) || !ValidID(entityID) {
		return nil, ErrEntityInvalid
	}
	cursor, err := s.coll.Find(ctx, bson.M{
		"tenant_id": tenantID, "entity_type": entityType, "entity_id": entityID,
	}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var out []Attachment
	for cursor.Next(ctx) {
		var d mongoDoc
		if err := cursor.Decode(&d); err != nil {
			return nil, err
		}
		out = append(out, d.toAttachment())
	}
	return out, cursor.Err()
}

// MarkReady flips a pending attachment to ready with the verified size and
// upload time. Already-ready attachments are left untouched.
func (s *Store) MarkReady(ctx context.Context, tenantID, id string, size int64, uploadedAt time.Time) error {
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": id, "tenant_id": tenantID, "status": StatusPending},
		bson.M{"$set": bson.M{"status": StatusReady, "size_bytes": size, "uploaded_at": uploadedAt}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		if _, err := s.Get(ctx, tenantID, id); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the metadata of one tenant's attachment.
func (s *Store) Delete(ctx context.Context, tenantID, id string) error {
	_, err := s.coll.DeleteOne(ctx, bson.M{"_id": id, "tenant_id": tenantID})
	return err
}

// ListPendingCreatedBefore sweeps pending metadata older than cutoff across
// tenants — the reaper's query; never exposed through the API.
func (s *Store) ListPendingCreatedBefore(ctx context.Context, before time.Time) ([]Attachment, error) {
	cursor, err := s.coll.Find(ctx, bson.M{"status": StatusPending, "created_at": bson.M{"$lt": before}})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var out []Attachment
	for cursor.Next(ctx) {
		var d mongoDoc
		if err := cursor.Decode(&d); err != nil {
			return nil, err
		}
		out = append(out, d.toAttachment())
	}
	return out, cursor.Err()
}
