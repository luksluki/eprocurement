package dlq

import (
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/services/worker/topology"
)

// deathTable builds an x-death header value shaped like the broker's (a
// slice of tables, newest first) — the widths RabbitMQ's own field tables
// decode to.
func deathTable(queue, reason string, diedAt time.Time) []any {
	return []any{
		amqp.Table{"queue": queue, "reason": reason, "time": diedAt, "count": int64(1)},
		amqp.Table{"queue": queue + ".retry.3", "reason": "expired", "time": diedAt.Add(-time.Minute), "count": int64(1)},
	}
}

func delivery(body []byte, headers amqp.Table, messageID string) amqp.Delivery {
	return amqp.Delivery{Body: body, Headers: headers, MessageId: messageID}
}

func envelopeBody(eventID, tenantID, eventType string) []byte {
	return []byte(`{"event_id":"` + eventID + `","tenant_id":"` + tenantID +
		`","type":"` + eventType + `","occurred_at":"2026-10-28T10:00:00Z"}`)
}

// parse covers the entry shapes List builds: decoded entries carry the
// business fields and the x-death/x-retry-count metadata; anything that
// does not unmarshal becomes a metadata-only decode_error entry whose
// tenant cannot be known. The x-retry-count column runs every integer
// width AMQP field tables decode to (topology.RetryCount's tolerance).
func TestParse(t *testing.T) {
	diedAt := time.Date(2026, 10, 28, 10, 5, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		body           []byte
		headers        amqp.Table
		wantDecodeErr  bool
		wantEventID    string
		wantTenantID   string
		wantType       string
		wantRetryCount int32
		wantQueue      string
		wantReason     string
		wantDiedAt     bool
	}{
		"decoded with full metadata": {
			body:        envelopeBody("evt-1", "tenant-a", "pr.submitted"),
			headers:     amqp.Table{"x-death": deathTable("documents.history", "rejected", diedAt), "x-retry-count": int32(3)},
			wantEventID: "evt-1", wantTenantID: "tenant-a", wantType: "pr.submitted",
			wantRetryCount: 3, wantQueue: "documents.history", wantReason: "rejected", wantDiedAt: true,
		},
		"undecodable body": {
			body:          []byte("{not json"),
			headers:       amqp.Table{"x-death": deathTable("notifications", "rejected", diedAt)},
			wantDecodeErr: true, wantQueue: "notifications", wantReason: "rejected", wantDiedAt: true,
		},
		"decoded but tenantless envelope (validation failure)": {
			body:        []byte(`{}`),
			headers:     amqp.Table{"x-death": deathTable("exports", "rejected", diedAt)},
			wantEventID: "", wantTenantID: "", wantQueue: "exports", wantReason: "rejected",
		},
		"retry count integer widths": {
			body: envelopeBody("evt-2", "tenant-a", "po.issued"),
			headers: amqp.Table{
				"x-death":       deathTable("notifications", "rejected", diedAt),
				"x-retry-count": int16(2),
			},
			wantEventID: "evt-2", wantTenantID: "tenant-a", wantType: "po.issued",
			wantRetryCount: 2, wantQueue: "notifications", wantReason: "rejected",
		},
		"retry count absent means zero": {
			body:        envelopeBody("evt-3", "tenant-b", "grn.posted"),
			headers:     amqp.Table{"x-death": deathTable("documents.history", "rejected", diedAt)},
			wantEventID: "evt-3", wantTenantID: "tenant-b", wantType: "grn.posted",
			wantRetryCount: 0, wantQueue: "documents.history", wantReason: "rejected",
		},
		"no x-death leaves queue and reason empty": {
			body:        envelopeBody("evt-4", "tenant-a", "pr.approved"),
			headers:     amqp.Table{"x-retry-count": int64(1)},
			wantEventID: "evt-4", wantTenantID: "tenant-a", wantType: "pr.approved",
			wantRetryCount: 1,
		},
		"garbage x-death is tolerated": {
			body:        envelopeBody("evt-5", "tenant-a", "pr.rejected"),
			headers:     amqp.Table{"x-death": "not-a-table", "x-retry-count": int(3)},
			wantEventID: "evt-5", wantTenantID: "tenant-a", wantType: "pr.rejected",
			wantRetryCount: 3,
		},
	} {
		m := parse(delivery(tc.body, tc.headers, "mid-"+tc.wantEventID))
		if m.DecodeError != tc.wantDecodeErr {
			t.Errorf("%s: DecodeError = %v, want %v", name, m.DecodeError, tc.wantDecodeErr)
		}
		if m.EventID != tc.wantEventID || m.TenantID != tc.wantTenantID || m.Type != tc.wantType {
			t.Errorf("%s: envelope = %q/%q/%q, want %q/%q/%q",
				name, m.EventID, m.TenantID, m.Type, tc.wantEventID, tc.wantTenantID, tc.wantType)
		}
		if m.RetryCount != tc.wantRetryCount {
			t.Errorf("%s: RetryCount = %d, want %d", name, m.RetryCount, tc.wantRetryCount)
		}
		if m.OriginalQueue != tc.wantQueue || m.Reason != tc.wantReason {
			t.Errorf("%s: death = %q/%q, want %q/%q",
				name, m.OriginalQueue, m.Reason, tc.wantQueue, tc.wantReason)
		}
		if tc.wantDiedAt && !m.DiedAt.Equal(diedAt) {
			t.Errorf("%s: DiedAt = %v, want %v", name, m.DiedAt, diedAt)
		}
	}
}

// The AMQP MessageId (the publisher's event_id) survives a broken body —
// it is the only identity a decode_error entry has.
func TestParseKeepsMessageIDOnDecodeError(t *testing.T) {
	m := parse(delivery([]byte("garbage"), nil, "evt-broken-1"))
	if !m.DecodeError || m.MessageID != "evt-broken-1" {
		t.Fatalf("parse = %+v, want decode_error with MessageID evt-broken-1", m)
	}
	if m.TenantID != "" || m.EventID != "" || m.Type != "" {
		t.Fatalf("decode_error entry leaked business fields: %+v", m)
	}
}

// Visibility filter: a tenant sees its own decoded entries plus every
// undecodable entry (cross-tenant, metadata only); another tenant's
// decoded entries are invisible to it.
func TestVisibilityFilter(t *testing.T) {
	owned := parse(delivery(envelopeBody("evt-a", "tenant-a", "pr.submitted"), nil, "evt-a"))
	foreign := parse(delivery(envelopeBody("evt-b", "tenant-b", "pr.submitted"), nil, "evt-b"))
	broken := parse(delivery([]byte("x"), nil, "evt-x"))
	tenantless := parse(delivery([]byte(`{}`), nil, "evt-n"))

	if !owned.Visible("tenant-a") || !owned.Owned("tenant-a") {
		t.Error("own decoded entry must be visible and owned")
	}
	if foreign.Visible("tenant-a") || foreign.Owned("tenant-a") {
		t.Error("another tenant's decoded entry must be invisible")
	}
	if !broken.Visible("tenant-a") || broken.Visible("does-not-matter") == false {
		t.Error("undecodable entries are visible to every tenant (metadata only)")
	}
	if broken.Owned("tenant-a") {
		t.Error("an undecodable entry is owned by nobody")
	}
	if tenantless.Visible("tenant-a") {
		t.Error("a decoded but tenantless entry is attributed to no tenant")
	}
}

// replayDecision is the pure replay gate: 404 for another tenant's message
// (cross-tenant opacity), 409 for a message that can never succeed
// (undecodable body, or no known target queue), nil for an owned message
// with a valid target.
func TestReplayDecision(t *testing.T) {
	targets := map[string]bool{}
	for _, q := range topology.Default.Queues {
		targets[q.Name] = true
	}
	diedAt := time.Now().UTC()
	owned := parse(delivery(envelopeBody("evt-a", "tenant-a", "pr.submitted"),
		amqp.Table{"x-death": deathTable(topology.DocumentsHistoryQueue, "rejected", diedAt)}, "evt-a"))
	foreign := parse(delivery(envelopeBody("evt-b", "tenant-b", "pr.submitted"),
		amqp.Table{"x-death": deathTable(topology.NotificationsQueue, "rejected", diedAt)}, "evt-b"))
	broken := parse(delivery([]byte("{"),
		amqp.Table{"x-death": deathTable(topology.NotificationsQueue, "rejected", diedAt)}, "evt-x"))
	tenantless := parse(delivery([]byte(`{}`),
		amqp.Table{"x-death": deathTable(topology.ExportsQueue, "rejected", diedAt)}, "evt-n"))
	noTarget := parse(delivery(envelopeBody("evt-t", "tenant-a", "pr.submitted"), nil, "evt-t"))
	oddTarget := parse(delivery(envelopeBody("evt-o", "tenant-a", "pr.submitted"),
		amqp.Table{"x-death": deathTable("documents.history.retry.3", "expired", diedAt)}, "evt-o"))

	for name, tc := range map[string]struct {
		msg  Message
		want error
	}{
		"owned with valid target":   {owned, nil},
		"another tenant's message":  {foreign, ErrMessageNotFound},
		"undecodable message":       {broken, ErrNotReplayable},
		"decoded but tenantless":    {tenantless, ErrMessageNotFound},
		"owned but no x-death":      {noTarget, ErrNotReplayable},
		"owned but retry-tier name": {oddTarget, ErrNotReplayable},
	} {
		err := replayDecision(tc.msg, "tenant-a", targets)
		if tc.want == nil && err != nil {
			t.Errorf("%s: replayDecision = %v, want nil", name, err)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: replayDecision = %v, want %v", name, err, tc.want)
		}
	}
}
