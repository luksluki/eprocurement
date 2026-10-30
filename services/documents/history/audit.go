package history

import (
	"bytes"
	"encoding/csv"
	"strings"
	"time"
)

// AuditAction reports whether an event type is a decision event and, if so,
// the audit action it records — the verbs of PRD §14.7 ("every decision
// (approve/reject/issue/post/match) writes audit_log"):
//
//   - approve/reject: the four decide event types the approval engine writes
//     (dev-note 031, mapped since #31);
//   - issue: po.issued (#36);
//   - post: grn.posted (#37);
//   - match: invoice.matched / invoice.discrepancy (#40) — both outcomes of
//     the match decision, one verb.
//
// That is eight decision event types over five verbs. The lifecycle verbs
// PRD §14.7 leaves out stay non-decisions: po.acknowledged (an obligation
// confirmed, not a decision), po.cancelled, po.closed, pr.submitted. The
// strings are repeated here instead of importing the procurement package —
// documents must not depend on the producer's domain, only on the envelope
// contract.
func AuditAction(eventType string) (action string, decision bool) {
	switch strings.TrimSpace(eventType) {
	case "pr.approved", "pr.step_approved":
		return "approve", true
	case "pr.rejected", "pr.step_rejected":
		return "reject", true
	case "po.issued":
		return "issue", true
	case "grn.posted":
		return "post", true
	case "invoice.matched", "invoice.discrepancy":
		return "match", true
	default:
		return "", false
	}
}

// AuditActions is the closed set of audit actions AuditAction produces —
// the ListAudit action filter validates against it.
var AuditActions = []string{"approve", "reject", "issue", "post", "match"}

// AuditEntry is one audit_log row: the decision's actor, action, entity,
// and trace, with details carrying the event payload verbatim (PRD §13:
// "actor, action, entity, before/after, trace_id" — the payload is the
// decision's record). ActorIP is the #44 extension: the acting client's IP
// as the gateway resolved it; empty on rows raised before #44. Like
// events, the collection is append-only: rows are only ever inserted,
// never updated or deleted (PRD §17).
type AuditEntry struct {
	EventID    string
	TenantID   string
	ActorID    string
	ActorIP    string
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
		ActorIP:    ev.ActorIP,
		Action:     action,
		EntityType: ev.EntityType,
		EntityID:   ev.EntityID,
		Details:    ev.PayloadJSON,
		TraceID:    ev.TraceID,
		OccurredAt: ev.OccurredAt,
		RecordedAt: recordedAt,
	}
}

// AuditCSVColumns is the pinned export header: bounded metadata columns
// only. Details (the raw payload JSON, potentially large) is deliberately
// excluded — with it the 10 000-row export could exceed the gRPC unary
// default; without it every column is bounded and the worst case stays a
// few MB (see #44's export bounds).
const AuditCSVColumns = "event_id,occurred_at,recorded_at,actor_id,actor_ip,action,entity_type,entity_id,trace_id"

// MaxAuditExportRows caps the CSV export at the newest N rows per tenant —
// the third leg of the export bounds (#44), alongside the bounded columns
// and the gateway's raised MaxCallRecvMsgSize.
const MaxAuditExportRows = 10000

// AuditCSV renders entries as RFC 4180 CSV with the pinned bounded columns
// (CRLF line endings, UTF-8 — the #49 export convention). Timestamps are
// RFC 3339 UTC.
func AuditCSV(entries []AuditEntry) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	_ = w.Write(strings.Split(AuditCSVColumns, ","))
	for _, e := range entries {
		_ = w.Write([]string{
			e.EventID,
			e.OccurredAt.UTC().Format(time.RFC3339),
			e.RecordedAt.UTC().Format(time.RFC3339),
			e.ActorID,
			e.ActorIP,
			e.Action,
			e.EntityType,
			e.EntityID,
			e.TraceID,
		})
	}
	w.Flush()
	return buf.Bytes()
}
