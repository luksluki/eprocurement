package history

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

var testBase = time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)

func validEvent(id string) Event {
	ev, err := NewEvent(id, "t-a", "pr.approved", testBase,
		"user-1", "trace-1", "requisition", "11111111-1111-1111-1111-111111111111", `{"number":"PR-000001"}`)
	if err != nil {
		panic(err)
	}
	return ev
}

func TestNewEventValidation(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want error
	}{
		{name: "blank event id", ev: func() Event {
			e := validEvent("e-1")
			e.EventID = " "
			return e
		}(), want: ErrEventIDInvalid},
		{name: "missing tenant", ev: func() Event {
			e := validEvent("e-1")
			e.TenantID = ""
			return e
		}(), want: ErrEventIncomplete},
		{name: "missing type", ev: func() Event {
			e := validEvent("e-1")
			e.Type = ""
			return e
		}(), want: ErrEventIncomplete},
		{name: "missing entity type", ev: func() Event {
			e := validEvent("e-1")
			e.EntityType = ""
			return e
		}(), want: ErrEventIncomplete},
		{name: "missing entity id", ev: func() Event {
			e := validEvent("e-1")
			e.EntityID = ""
			return e
		}(), want: ErrEventIncomplete},
		{name: "zero occurred_at", ev: func() Event {
			e := validEvent("e-1")
			e.OccurredAt = time.Time{}
			return e
		}(), want: ErrEventIncomplete},
		{name: "payload not json", ev: func() Event {
			e := validEvent("e-1")
			e.PayloadJSON = "not json"
			return e
		}(), want: ErrPayloadInvalid},
		{name: "payload object", ev: validEvent("e-1"), want: nil},
		{name: "payload empty object", ev: func() Event {
			e := validEvent("e-1")
			e.PayloadJSON = "{}"
			return e
		}(), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ev.Validate()
			if !errors.Is(err, tt.want) {
				t.Fatalf("Validate error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNewEventDefaultsAndTrims(t *testing.T) {
	ev, err := NewEvent("  e-1  ", "  t-a ", " pr.approved\t", testBase,
		" user-1 ", " trace-1 ", " requisition ", " r-1 ", `{"ok":true}`)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.EventID != "e-1" || ev.TenantID != "t-a" || ev.Type != "pr.approved" ||
		ev.ActorID != "user-1" || ev.EntityType != "requisition" || ev.EntityID != "r-1" {
		t.Fatalf("trim failed: %+v", ev)
	}
	if ev.Version != 1 {
		t.Fatalf("Version = %d, want the default 1", ev.Version)
	}
}

// TestAuditAction pins which event types are decisions and the audit action
// they record — the four decide event types of dev-note 031, nothing else.
func TestAuditAction(t *testing.T) {
	tests := []struct {
		eventType    string
		wantAction   string
		wantDecision bool
	}{
		{"pr.approved", "approve", true},
		{"pr.step_approved", "approve", true},
		{"pr.rejected", "reject", true},
		{"pr.step_rejected", "reject", true},
		{"pr.submitted", "", false},
		{"pr.created", "", false},
		{"po.issued", "", false},
		{"notification.sent", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		action, decision := AuditAction(tt.eventType)
		if action != tt.wantAction || decision != tt.wantDecision {
			t.Fatalf("AuditAction(%q) = (%q, %v), want (%q, %v)",
				tt.eventType, action, decision, tt.wantAction, tt.wantDecision)
		}
	}
}

func TestNewAuditEntryDerivesFromEvent(t *testing.T) {
	ev := validEvent("e-1")
	recorded := testBase.Add(time.Minute)
	entry := NewAuditEntry(ev, "approve", recorded)
	want := AuditEntry{
		EventID: ev.EventID, TenantID: ev.TenantID, ActorID: ev.ActorID,
		Action: "approve", EntityType: ev.EntityType, EntityID: ev.EntityID,
		Details: ev.PayloadJSON, TraceID: ev.TraceID,
		OccurredAt: ev.OccurredAt, RecordedAt: recorded,
	}
	if entry != want {
		t.Fatalf("NewAuditEntry = %+v, want %+v", entry, want)
	}
}

// TestEventBSONShape pins the stored document's field names — the shape
// docs/schema/documents.md documents. The type field is `type` (not
// event_type), matching the PRD §14.5 envelope and the seed.
func TestEventBSONShape(t *testing.T) {
	ev := validEvent("e-1")
	doc := EventBSON(ev)
	wantKeys := []string{
		"event_id", "tenant_id", "type", "version", "occurred_at",
		"actor_id", "trace_id", "entity_type", "entity_id", "payload",
	}
	if len(doc) != len(wantKeys) {
		t.Fatalf("EventBSON has %d keys, want %d: %v", len(doc), len(wantKeys), doc)
	}
	for _, k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Fatalf("EventBSON missing key %q: %v", k, doc)
		}
	}
	if doc["type"] != ev.Type || doc["version"] != int32(1) || doc["payload"] != ev.PayloadJSON {
		t.Fatalf("EventBSON values = %v", doc)
	}
}

func TestAuditBSONShape(t *testing.T) {
	entry := NewAuditEntry(validEvent("e-1"), "reject", testBase)
	doc := AuditBSON(entry)
	wantKeys := []string{
		"event_id", "tenant_id", "actor_id", "action", "entity_type",
		"entity_id", "details", "trace_id", "occurred_at", "recorded_at",
	}
	if len(doc) != len(wantKeys) {
		t.Fatalf("AuditBSON has %d keys, want %d: %v", len(doc), len(wantKeys), doc)
	}
	for _, k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Fatalf("AuditBSON missing key %q: %v", k, doc)
		}
	}
}

// TestCursorRoundTrip pins the cursor codec: the keyset position survives
// an encode/decode pair at MongoDB's millisecond precision.
func TestCursorRoundTrip(t *testing.T) {
	at := testBase.Add(1234 * time.Millisecond) // sub-millisecond is not stored
	cursor := encodeCursor(at, "e-42")
	gotAt, gotID, err := decodeCursor(cursor)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !gotAt.Equal(at) || gotID != "e-42" {
		t.Fatalf("round trip = (%v, %q), want (%v, %q)", gotAt, gotID, at, "e-42")
	}
}

func TestCursorInvalid(t *testing.T) {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tests := []string{
		"not base64!",
		b64("no-separator"),
		b64("not-a-number|e-1"),
		b64("123|"), // empty event id
		b64("|e-1"), // empty timestamp
	}
	for _, c := range tests {
		if _, _, err := decodeCursor(c); err == nil {
			t.Fatalf("decodeCursor(%q) succeeded, want error", c)
		}
	}
}

// TestListFilterNormalize pins limit normalization (vendor-list convention:
// below 1 → default, above max → max) and the both-or-neither entity rule.
func TestListFilterNormalize(t *testing.T) {
	f, err := ListFilter{Limit: 0}.Validate()
	if err != nil || f.Limit != DefaultLimit {
		t.Fatalf("zero limit: (%d, %v), want default %d", f.Limit, err, DefaultLimit)
	}
	f, err = ListFilter{Limit: -5}.Validate()
	if err != nil || f.Limit != DefaultLimit {
		t.Fatalf("negative limit: (%d, %v), want default %d", f.Limit, err, DefaultLimit)
	}
	f, err = ListFilter{Limit: 1000}.Validate()
	if err != nil || f.Limit != MaxLimit {
		t.Fatalf("over-max limit: (%d, %v), want max %d", f.Limit, err, MaxLimit)
	}
	f, err = ListFilter{EntityType: "requisition"}.Validate()
	if !errors.Is(err, ErrFilterInvalid) {
		t.Fatalf("entity type without id: %v, want ErrFilterInvalid", err)
	}
	f, err = ListFilter{EntityID: "r-1"}.Validate()
	if !errors.Is(err, ErrFilterInvalid) {
		t.Fatalf("entity id without type: %v, want ErrFilterInvalid", err)
	}
	f, err = ListFilter{EntityType: " requisition ", EntityID: " r-1 ", Cursor: "junk"}.Validate()
	if !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("invalid cursor: %v, want ErrCursorInvalid", err)
	}
	f, err = ListFilter{EntityType: " requisition ", EntityID: " r-1 "}.Validate()
	if err != nil || f.EntityType != "requisition" || f.EntityID != "r-1" {
		t.Fatalf("trim failed: (%+v, %v)", f, err)
	}
}
