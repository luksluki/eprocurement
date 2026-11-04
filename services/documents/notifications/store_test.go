package notifications

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/documents/internal/testenv"
)

// TestRecipient pins the recipient-derivation contract of the notification
// consumer (issue #47): payload.requester_id first, payload.submitted_by as
// fallback, empty when neither is present or the payload carries no object.
func TestRecipient(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"requester_id wins", `{"requester_id":"user-1","submitted_by":"user-2"}`, "user-1"},
		{"submitted_by fallback", `{"submitted_by":"user-2"}`, "user-2"},
		{"requester_id blank falls back", `{"requester_id":"  ","submitted_by":"user-2"}`, "user-2"},
		{"both absent", `{"number":"PR-000001"}`, ""},
		{"empty object", `{}`, ""},
		{"non-object payload", `["not","an","object"]`, ""},
		{"invalid json", `{{`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Recipient(tc.payload); got != tc.want {
				t.Fatalf("Recipient(%s) = %q, want %q", tc.payload, got, tc.want)
			}
		})
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	db := testenv.Mongo(t)
	store := NewStore(db)
	if err := store.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	return store
}

func validEvent(eventID string) history.Event {
	return history.Event{
		EventID: eventID, TenantID: "t-47", Type: "pr.approved",
		OccurredAt: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
		EntityType: "requisition", EntityID: "req-1",
		PayloadJSON: `{"requester_id":"user-1","number":"PR-000001","decision":"approve"}`,
	}
}

// TestApplyIdempotent pins the consumer-dedupe contract: applying the same
// event twice leaves exactly one notifications document; the replay reports
// applied=false and no error (PRD §14.5, unique event_id index).
func TestApplyIdempotent(t *testing.T) {
	s := newStore(t)
	ev := validEvent("eeeeeeee-0000-0000-0000-000000000047")

	applied, err := s.Apply(t.Context(), ev)
	if err != nil || !applied {
		t.Fatalf("first Apply = (%v, %v), want (true, nil)", applied, err)
	}
	applied, err = s.Apply(t.Context(), ev)
	if err != nil || applied {
		t.Fatalf("replay Apply = (%v, %v), want (false, nil)", applied, err)
	}
	n, err := s.coll.CountDocuments(t.Context(), bson.M{"tenant_id": ev.TenantID})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("documents after replay = %d, want 1", n)
	}
}

// TestApplyStoresEnvelope pins the stored shape: every envelope field is
// projected and recipient_id follows the requester_id/submitted_by rule —
// including the "no recipient, document still created" case.
func TestApplyStoresEnvelope(t *testing.T) {
	s := newStore(t)

	cases := []struct {
		name          string
		payload       string
		wantRecipient string
	}{
		{"requester_id", `{"requester_id":"user-1"}`, "user-1"},
		{"submitted_by fallback", `{"submitted_by":"user-2"}`, "user-2"},
		{"no recipient still creates the document", `{"number":"PR-000001"}`, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := validEvent(fmt.Sprintf("eeeeeeee-0000-0000-0000-00000000005%d", i))
			ev.PayloadJSON = tc.payload
			applied, err := s.Apply(t.Context(), ev)
			if err != nil || !applied {
				t.Fatalf("Apply = (%v, %v), want (true, nil)", applied, err)
			}
			var doc struct {
				EventID     string    `bson:"event_id"`
				Type        string    `bson:"type"`
				RecipientID string    `bson:"recipient_id"`
				Payload     string    `bson:"payload"`
				RecordedAt  time.Time `bson:"recorded_at"`
			}
			if err := s.coll.FindOne(t.Context(), bson.M{"event_id": ev.EventID}).Decode(&doc); err != nil {
				t.Fatalf("find: %v", err)
			}
			if doc.RecipientID != tc.wantRecipient {
				t.Fatalf("recipient_id = %q, want %q", doc.RecipientID, tc.wantRecipient)
			}
			if doc.Type != ev.Type || doc.Payload != tc.payload {
				t.Fatalf("stored type/payload = %q/%q, want %q/%q", doc.Type, doc.Payload, ev.Type, tc.payload)
			}
			if doc.RecordedAt.IsZero() {
				t.Fatal("recorded_at not set")
			}
		})
	}
}

// TestApplyValidationFailures pins the permanent-invalid boundary: events
// that fail envelope validation error out (no document written) with the
// history sentinel errors intact, so the consumer can classify them as
// permanent and dead-letter without retry (issue #47 AC).
func TestApplyValidationFailures(t *testing.T) {
	s := newStore(t)
	cases := []struct {
		name string
		mut  func(*history.Event)
		want error
	}{
		{"missing entity_type", func(e *history.Event) { e.EntityType = "" }, history.ErrEventIncomplete},
		{"missing entity_id", func(e *history.Event) { e.EntityID = "" }, history.ErrEventIncomplete},
		{"missing tenant_id", func(e *history.Event) { e.TenantID = "" }, history.ErrEventIncomplete},
		{"missing event_id", func(e *history.Event) { e.EventID = " " }, history.ErrEventIDInvalid},
		{"invalid payload json", func(e *history.Event) { e.PayloadJSON = `{"no"` }, history.ErrPayloadInvalid},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := validEvent(fmt.Sprintf("eeeeeeee-0000-0000-0000-00000000006%d", i))
			tc.mut(&ev)
			_, err := s.Apply(t.Context(), ev)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Apply error = %v, want %v", err, tc.want)
			}
		})
	}
	n, err := s.coll.CountDocuments(t.Context(), bson.M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("documents after invalid applies = %d, want 0", n)
	}
}

// TestEnsureIndexesPinned proves the boot path creates exactly the pinned
// index set on a fresh database: unique uniq_event_id plus the compound
// tenant_occurred {tenant_id, occurred_at} (PRD §14.7).
func TestEnsureIndexesPinned(t *testing.T) {
	s := newStore(t)
	cur, err := s.coll.Indexes().List(t.Context())
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var found []bson.M
	if err := cur.All(t.Context(), &found); err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	byName := map[string]bson.M{}
	for _, ix := range found {
		byName[ix["name"].(string)] = ix
	}
	uniq, ok := byName["uniq_event_id"]
	if !ok {
		t.Fatalf("uniq_event_id missing, indexes: %v", byName)
	}
	if uniq["unique"] != true {
		t.Fatalf("uniq_event_id not unique: %v", uniq)
	}
	if got := fmt.Sprint(uniq["key"]); got != "map[event_id:1]" && got != "[{event_id 1}]" {
		t.Fatalf("uniq_event_id keys = %v", uniq["key"])
	}
	occ, ok := byName["tenant_occurred"]
	if !ok {
		t.Fatalf("tenant_occurred missing, indexes: %v", byName)
	}
	if got := fmt.Sprint(occ["key"]); got != "map[occurred_at:-1 tenant_id:1]" && got != "[{tenant_id 1} {occurred_at -1}]" {
		t.Fatalf("tenant_occurred keys = %v", occ["key"])
	}
	if len(byName) != 3 { // _id_ + the two pinned
		t.Fatalf("indexes = %v, want exactly _id_ + uniq_event_id + tenant_occurred", byName)
	}
}
