package history

import (
	"strings"
	"time"
)

// AuditAction reports whether an event type is a decision event and, if so,
// the audit action it records. The four decide event types are the ones the
// approval engine writes to the outbox (services/procurement/approval
// Outbox* constants, dev-note 031); the strings are repeated here instead
// of importing the procurement package — documents must not depend on the
// producer's domain, only on the envelope contract.
func AuditAction(eventType string) (action string, decision bool) {
	switch strings.TrimSpace(eventType) {
	case "pr.approved", "pr.step_approved":
		return "approve", true
	case "pr.rejected", "pr.step_rejected":
		return "reject", true
	default:
		return "", false
	}
}

// AuditEntry is one audit_log row: the decision's actor, action, entity,
// and trace, with details carrying the event payload verbatim (PRD §13:
// "actor, action, entity, before/after, trace_id" — the payload is the
// decision's record). Like events, the collection is append-only: rows are
// only ever inserted, never updated or deleted (PRD §17).
type AuditEntry struct {
	EventID    string
	TenantID   string
	ActorID    string
	Action     string
	EntityType string
	EntityID   string
	Details    string // raw JSON payload of the event
	TraceID    string
	OccurredAt time.Time
	RecordedAt time.Time
}

// NewAuditEntry derives the audit row of a decision event. recordedAt is
// the projection time (when Apply wrote the row), distinct from the
// business time occurred_at.
func NewAuditEntry(ev Event, action string, recordedAt time.Time) AuditEntry {
	return AuditEntry{
		EventID:    ev.EventID,
		TenantID:   ev.TenantID,
		ActorID:    ev.ActorID,
		Action:     action,
		EntityType: ev.EntityType,
		EntityID:   ev.EntityID,
		Details:    ev.PayloadJSON,
		TraceID:    ev.TraceID,
		OccurredAt: ev.OccurredAt,
		RecordedAt: recordedAt,
	}
}
