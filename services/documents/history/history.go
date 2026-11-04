// Package history is the documents service's append-only event store
// (PRD §9 "history is append-only", §13, ADR-0001): it consumes procurement
// domain events idempotently into MongoDB `events` and mirrors every
// decision event into `audit_log` (PRD §14.7). There is deliberately no
// update or delete path — both collections grow only through Apply, and the
// only exports are EnsureIndexes, the document constructors, Apply, and
// List. Transport (RabbitMQ) and the polling consumer live with the worker
// issues (#45–#47); Apply is the projection function they will call.
package history

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Collection names in the documents MongoDB database (PRD §13).
const (
	EventsCollectionName = "events"
	AuditCollectionName  = "audit_log"
)

// Machine-readable failure codes — the gRPC message text at the API
// boundary, following the attachment package's convention.
var (
	ErrEventIDInvalid  = errors.New("events_event_id_invalid")
	ErrEventIncomplete = errors.New("events_event_incomplete")
	ErrPayloadInvalid  = errors.New("events_payload_invalid")
	ErrFilterInvalid   = errors.New("events_filter_invalid")
	ErrCursorInvalid   = errors.New("events_cursor_invalid")
	ErrActionInvalid   = errors.New("audit_action_invalid")
)

// DefaultLimit and MaxLimit bound ListEvents pages. Limit normalization
// follows the vendor-list convention (vendor.ValidateList): values below 1
// fall back to the default, values above the max clamp to it.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Event is one domain event as projected into the events collection — the
// PRD §14.5 envelope with the payload carried as raw JSON. The bson field
// names are pinned in EventBSON and documented in docs/schema/documents.md;
// note the type field is `type`, not `event_type`, matching the envelope
// and the seed. PayloadJSON must be valid JSON (the outbox jsonb guarantee)
// so consumers can parse it without trusting the producer. ActorIP is the
// documented §14.5 envelope extension of #44 (the acting client's IP as the
// gateway resolved it); empty on events raised before it existed.
type Event struct {
	EventID     string
	TenantID    string
	Type        string
	Version     int32
	OccurredAt  time.Time
	ActorID     string
	ActorIP     string
	TraceID     string
	EntityType  string
	EntityID    string
	PayloadJSON string
}

// NewEvent is the Event constructor: it trims the string fields, defaults
// Version to 1 (the only payload schema version today, as the outbox
// writer does), and validates the result. The seed uses it so the seeded
// documents can never drift from what Apply writes.
func NewEvent(eventID, tenantID, eventType string, occurredAt time.Time,
	actorID, actorIP, traceID, entityType, entityID, payloadJSON string) (Event, error) {
	ev := Event{
		EventID:     eventID,
		TenantID:    tenantID,
		Type:        eventType,
		OccurredAt:  occurredAt,
		ActorID:     actorID,
		ActorIP:     actorIP,
		TraceID:     traceID,
		EntityType:  entityType,
		EntityID:    entityID,
		PayloadJSON: payloadJSON,
	}.normalize()
	return ev, ev.Validate()
}

// normalize trims the wire strings and defaults the version in place, so
// both NewEvent and Apply store canonical values whatever the caller built.
func (e Event) normalize() Event {
	e.EventID = strings.TrimSpace(e.EventID)
	e.TenantID = strings.TrimSpace(e.TenantID)
	e.Type = strings.TrimSpace(e.Type)
	e.ActorID = strings.TrimSpace(e.ActorID)
	e.ActorIP = strings.TrimSpace(e.ActorIP)
	e.TraceID = strings.TrimSpace(e.TraceID)
	e.EntityType = strings.TrimSpace(e.EntityType)
	e.EntityID = strings.TrimSpace(e.EntityID)
	if e.Version <= 0 {
		e.Version = 1
	}
	return e
}

// Validate checks the invariants Apply depends on: identity fields present
// (after trimming, so a directly-constructed Event passes the same checks a
// NewEvent-built one does), a point-in-time for occurred_at, and a
// well-formed JSON payload.
func (e Event) Validate() error {
	if strings.TrimSpace(e.EventID) == "" {
		return ErrEventIDInvalid
	}
	if strings.TrimSpace(e.TenantID) == "" || strings.TrimSpace(e.Type) == "" ||
		strings.TrimSpace(e.EntityType) == "" || strings.TrimSpace(e.EntityID) == "" {
		return ErrEventIncomplete
	}
	if e.Version <= 0 {
		e.Version = 1
	}
	if e.OccurredAt.IsZero() {
		return ErrEventIncomplete
	}
	if !json.Valid([]byte(e.PayloadJSON)) {
		return ErrPayloadInvalid
	}
	return nil
}

// AuditEntry, AuditAction, and NewAuditEntry — the audit_log projection —
// live in audit.go.
