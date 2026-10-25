// Package notifications is the documents service's notification projection
// (issue #47; PRD §14.5 queue `notifications`, §14.7): it consumes the
// actor-facing subset of the procurement event catalog idempotently into
// the MongoDB `notifications` collection — exactly one document per
// event_id, with the recipient derived from the event payload. The queue
// bindings (which events reach this projection) are the worker consumer's
// concern; like history, this package is only the projection function the
// consumer calls.
package notifications

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/luksluki/eprocurement/services/documents/history"
)

// NotificationsCollectionName is the collection name in the documents
// MongoDB database (PRD §14.5 names the queue; the collection mirrors it).
const NotificationsCollectionName = "notifications"

// Notification is one stored notification: the PRD §14.5 envelope plus the
// derived recipient and the projection timestamp. The bson field names are
// pinned in NotificationBSON and documented in docs/schema/documents.md.
type Notification struct {
	EventID     string
	TenantID    string
	Type        string
	Version     int32
	OccurredAt  time.Time
	ActorID     string
	TraceID     string
	EntityType  string
	EntityID    string
	RecipientID string    // derived: payload.requester_id, fallback payload.submitted_by; may be empty
	PayloadJSON string    // raw JSON payload of the event, verbatim
	RecordedAt  time.Time // projection time (when Apply wrote the document)
}

// Recipient extracts the notification recipient from an event payload:
// `requester_id` when present (the requisition submitter the approval
// decision is about), falling back to `submitted_by` (the actor of the
// pr.submitted event). Both may be absent — the caller then stores an empty
// recipient and the notification document is still created: the effect is
// durable per event either way, and the read side can decide what to do
// with recipient-less rows.
func Recipient(payloadJSON string) string {
	var p struct {
		RequesterID string `json:"requester_id"`
		SubmittedBy string `json:"submitted_by"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return ""
	}
	if v := strings.TrimSpace(p.RequesterID); v != "" {
		return v
	}
	return strings.TrimSpace(p.SubmittedBy)
}

// normalize mirrors history.Event.normalize (unexported there) so Apply
// stores canonical trimmed values whatever the caller built.
func normalize(ev history.Event) history.Event {
	ev.EventID = strings.TrimSpace(ev.EventID)
	ev.TenantID = strings.TrimSpace(ev.TenantID)
	ev.Type = strings.TrimSpace(ev.Type)
	ev.ActorID = strings.TrimSpace(ev.ActorID)
	ev.TraceID = strings.TrimSpace(ev.TraceID)
	ev.EntityType = strings.TrimSpace(ev.EntityType)
	ev.EntityID = strings.TrimSpace(ev.EntityID)
	if ev.Version <= 0 {
		ev.Version = 1
	}
	return ev
}

// NewNotification derives the notification document of one event at
// recordedAt (the projection time).
func NewNotification(ev history.Event, recordedAt time.Time) Notification {
	return Notification{
		EventID:     ev.EventID,
		TenantID:    ev.TenantID,
		Type:        ev.Type,
		Version:     ev.Version,
		OccurredAt:  ev.OccurredAt,
		ActorID:     ev.ActorID,
		TraceID:     ev.TraceID,
		EntityType:  ev.EntityType,
		EntityID:    ev.EntityID,
		RecipientID: Recipient(ev.PayloadJSON),
		PayloadJSON: ev.PayloadJSON,
		RecordedAt:  recordedAt,
	}
}
