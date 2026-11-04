package history

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/luksluki/eprocurement/services/documents/internal/testenv"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db := testenv.Mongo(t)
	store := NewStore(db)
	if err := store.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	return store
}

func countDocs(t *testing.T, s *Store, collection, tenantID string) int64 {
	t.Helper()
	n, err := s.events.Database().Collection(collection).CountDocuments(t.Context(),
		bson.M{"tenant_id": tenantID})
	if err != nil {
		t.Fatalf("count %s: %v", collection, err)
	}
	return n
}

// TestApplyIdempotent pins the consumer-dedupe contract: applying the same
// event twice leaves exactly one events document and — for decision event
// types — exactly one audit_log row; the replay reports applied=false and
// no error (PRD §14.5, dedupe via the unique event_id index).
func TestApplyIdempotent(t *testing.T) {
	s := newStore(t)

	ev := validEvent("eeeeeeee-0000-0000-0000-000000000001")
	ev.PayloadJSON = `{"number":"PR-000001","decision":"approve"}`

	applied, err := s.Apply(t.Context(), ev)
	if err != nil || !applied {
		t.Fatalf("first Apply = (%v, %v), want (true, nil)", applied, err)
	}
	// A non-decision event must not create audit rows.
	if n := countDocs(t, s, AuditCollectionName, ev.TenantID); n != 1 {
		t.Fatalf("audit rows after decision = %d, want 1", n)
	}

	applied, err = s.Apply(t.Context(), ev)
	if err != nil || applied {
		t.Fatalf("replay Apply = (%v, %v), want (false, nil)", applied, err)
	}
	if n := countDocs(t, s, EventsCollectionName, ev.TenantID); n != 1 {
		t.Fatalf("events after replay = %d, want 1", n)
	}
	if n := countDocs(t, s, AuditCollectionName, ev.TenantID); n != 1 {
		t.Fatalf("audit rows after replay = %d, want 1", n)
	}

	// A non-decision event writes no audit row at all.
	plain, err := NewEvent("eeeeeeee-0000-0000-0000-000000000002", ev.TenantID,
		"pr.submitted", testBase.Add(time.Second), "user-1", "203.0.113.8", "trace-1",
		"requisition", ev.EntityID, `{"number":"PR-000001"}`)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if _, err := s.Apply(t.Context(), plain); err != nil {
		t.Fatalf("Apply plain: %v", err)
	}
	if n := countDocs(t, s, EventsCollectionName, ev.TenantID); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
	if n := countDocs(t, s, AuditCollectionName, ev.TenantID); n != 1 {
		t.Fatalf("audit rows after non-decision event = %d, want 1", n)
	}
}

// TestApplyPartialRecovery pins recovery from a crash between the two
// inserts. The local MongoDB is a single-node mongod without a replica
// set, so Apply cannot wrap them in a transaction; instead each insert is
// idempotent on event_id. Simulating the failure: the event landed, the
// audit write did not (the row is missing). Re-applying completes the
// projection — the audit row appears, the event is not duplicated.
func TestApplyPartialRecovery(t *testing.T) {
	s := newStore(t)

	ev := validEvent("eeeeeeee-0000-0000-0000-000000000010")
	if _, err := s.Apply(t.Context(), ev); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The audit write "failed": drop the row the first Apply wrote.
	if _, err := s.audit.DeleteOne(t.Context(), bson.M{"event_id": ev.EventID}); err != nil {
		t.Fatalf("simulate failed audit write: %v", err)
	}
	if n := countDocs(t, s, AuditCollectionName, ev.TenantID); n != 0 {
		t.Fatalf("audit rows after simulated failure = %d, want 0", n)
	}

	// The consumer retries the same event.
	applied, err := s.Apply(t.Context(), ev)
	if err != nil {
		t.Fatalf("retry Apply: %v", err)
	}
	if applied {
		t.Fatalf("retry reported the event as fresh; want replay")
	}
	if n := countDocs(t, s, EventsCollectionName, ev.TenantID); n != 1 {
		t.Fatalf("events after retry = %d, want 1 (no duplicate)", n)
	}
	if n := countDocs(t, s, AuditCollectionName, ev.TenantID); n != 1 {
		t.Fatalf("audit rows after retry = %d, want 1 (recovered)", n)
	}
}

// TestApplyInvalid pins the typed validation failures before any write.
func TestApplyInvalid(t *testing.T) {
	s := newStore(t)
	bad := validEvent("e-1")
	bad.EventID = ""
	if _, err := s.Apply(t.Context(), bad); err != ErrEventIDInvalid {
		t.Fatalf("Apply blank id = %v, want ErrEventIDInvalid", err)
	}
	if n := countDocs(t, s, EventsCollectionName, "t-a"); n != 0 {
		t.Fatalf("events after invalid apply = %d, want 0", n)
	}
}

// appliedSeed builds a deterministic tenant history: five events where the
// two newest share occurred_at (the approval engine's step + terminal pair
// from approval/store.go) plus a stale one, ordered by event_id within the
// tie. ids run e5 > e4 > ... > e1 so the expected order is e5, e4, e3, e2, e1.
func appliedSeed(t *testing.T, s *Store, tenant string) []Event {
	t.Helper()
	specs := []struct {
		id, eventType string
		at            time.Time
		entity        string
	}{
		{"e5", "pr.step_approved", testBase.Add(3 * time.Second), "r-1"},
		{"e4", "pr.approved", testBase.Add(3 * time.Second), "r-1"}, // tie with e5
		{"e3", "pr.submitted", testBase.Add(2 * time.Second), "r-1"},
		{"e2", "pr.created", testBase.Add(1 * time.Second), "r-2"},
		{"e1", "pr.created", testBase, "r-2"},
	}
	events := make([]Event, 0, len(specs))
	for _, sp := range specs {
		ev, err := NewEvent(sp.id, tenant, sp.eventType, sp.at,
			"user-1", "203.0.113.7", "trace-1", "requisition", sp.entity, `{"number":"PR-000001"}`)
		if err != nil {
			t.Fatalf("NewEvent %s: %v", sp.id, err)
		}
		if _, err := s.Apply(t.Context(), ev); err != nil {
			t.Fatalf("Apply %s: %v", sp.id, err)
		}
		events = append(events, ev)
	}
	return events
}

func eventIDs(events []Event) []string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.EventID)
	}
	return ids
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestListNewestFirstTiebreak pins the total order: occurred_at DESC with
// event_id DESC as the tiebreaker, so the decide pair sharing occurred_at
// (e5 then e4) comes back in a stable order.
func TestListNewestFirstTiebreak(t *testing.T) {
	s := newStore(t)
	appliedSeed(t, s, "t-a")

	events, next, err := s.List(t.Context(), "t-a", ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"e5", "e4", "e3", "e2", "e1"}
	if !equalIDs(eventIDs(events), want) {
		t.Fatalf("order = %v, want %v", eventIDs(events), want)
	}
	if next != "" {
		t.Fatalf("next cursor on the last page = %q, want empty", next)
	}
	if events[0].Version != 1 || events[0].ActorID != "user-1" ||
		events[0].TraceID != "trace-1" || events[0].PayloadJSON != `{"number":"PR-000001"}` {
		t.Fatalf("round trip lost fields: %+v", events[0])
	}
}

// TestListCursorPagination walks the whole history page by page: every
// event appears exactly once, in order, across pages, and the final page's
// next_cursor is empty.
func TestListCursorPagination(t *testing.T) {
	s := newStore(t)
	appliedSeed(t, s, "t-a")

	var walked []string
	pages := 0
	cursor := ""
	for {
		events, next, err := s.List(t.Context(), "t-a", ListFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("List page %d: %v", pages+1, err)
		}
		walked = append(walked, eventIDs(events)...)
		pages++
		if next == "" {
			break
		}
		cursor = next
	}
	want := []string{"e5", "e4", "e3", "e2", "e1"}
	if !equalIDs(walked, want) {
		t.Fatalf("walked pages = %v, want %v (no duplicates, no skips)", walked, want)
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
}

// TestListEntityFilterAndCrossTenant pins the tenant scoping and the
// aggregate filter: another tenant's history is invisible, and the entity
// filter narrows to one aggregate within the caller's tenant.
func TestListEntityFilterAndCrossTenant(t *testing.T) {
	s := newStore(t)
	appliedSeed(t, s, "t-a")
	other, err := NewEvent("f1", "t-b", "pr.created", testBase.Add(time.Hour),
		"user-b", "198.51.100.9", "trace-b", "requisition", "r-1", `{"number":"PR-000009"}`)
	if err != nil {
		t.Fatalf("NewEvent tenant B: %v", err)
	}
	if _, err := s.Apply(t.Context(), other); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	// Cross-tenant denial: tenant B sees none of tenant A's events.
	events, _, err := s.List(t.Context(), "t-b", ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List tenant B: %v", err)
	}
	if len(events) != 1 || events[0].TenantID != "t-b" {
		t.Fatalf("tenant B sees %v, want only its own event", eventIDs(events))
	}

	// Entity filter within the tenant.
	events, _, err = s.List(t.Context(), "t-a", ListFilter{EntityType: "requisition", EntityID: "r-1", Limit: 10})
	if err != nil {
		t.Fatalf("List filtered: %v", err)
	}
	if !equalIDs(eventIDs(events), []string{"e5", "e4", "e3"}) {
		t.Fatalf("filtered order = %v", eventIDs(events))
	}

	// Both-or-neither is enforced at the store too (defense in depth —
	// the API layer validates first).
	if _, _, err := s.List(t.Context(), "t-a", ListFilter{EntityType: "requisition"}); err != ErrFilterInvalid {
		t.Fatalf("half filter error = %v, want ErrFilterInvalid", err)
	}
	if _, _, err := s.List(t.Context(), "t-a", ListFilter{Cursor: "junk"}); err != ErrCursorInvalid {
		t.Fatalf("bad cursor error = %v, want ErrCursorInvalid", err)
	}
}

// auditSeed applies one decision event of each of the five verbs plus one
// non-decision, with distinct occurred_at stamps, and returns the ids in
// expected newest-first order. Tenant t-a; actor ips alternate so the
// actor_ip round trip is assertable.
func auditSeed(t *testing.T, s *Store) {
	t.Helper()
	specs := []struct {
		id, eventType, actorIP string
		at                     time.Time
	}{
		{"a1", "pr.approved", "203.0.113.1", testBase},
		{"a2", "pr.step_rejected", "203.0.113.2", testBase.Add(1 * time.Second)},
		{"a3", "po.issued", "203.0.113.3", testBase.Add(2 * time.Second)},
		{"a4", "grn.posted", "203.0.113.4", testBase.Add(3 * time.Second)},
		{"a5", "invoice.matched", "203.0.113.5", testBase.Add(4 * time.Second)},
		{"a6", "invoice.discrepancy", "203.0.113.6", testBase.Add(5 * time.Second)},
		{"a7", "po.acknowledged", "203.0.113.7", testBase.Add(6 * time.Second)}, // not a decision
	}
	for _, sp := range specs {
		ev, err := NewEvent(sp.id, "t-a", sp.eventType, sp.at,
			"user-1", sp.actorIP, "trace-44", "requisition", "r-1", `{"number":"PR-000001"}`)
		if err != nil {
			t.Fatalf("NewEvent %s: %v", sp.id, err)
		}
		if _, err := s.Apply(t.Context(), ev); err != nil {
			t.Fatalf("Apply %s: %v", sp.id, err)
		}
	}
}

// TestListAudit pins the audit read side (#44): the decision projection now
// covers all five verbs, actor_ip round-trips into the rows, ordering is
// newest-first with the event_id tiebreaker, and the action filter narrows.
func TestListAudit(t *testing.T) {
	s := newStore(t)
	auditSeed(t, s)

	entries, next, err := s.ListAudit(t.Context(), "t-a", AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	// a7 (po.acknowledged) is not a decision: six audit rows, newest first.
	want := []string{"a6", "a5", "a4", "a3", "a2", "a1"}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.EventID)
	}
	if !equalIDs(got, want) {
		t.Fatalf("ListAudit order = %v, want %v", got, want)
	}
	if next != "" {
		t.Fatalf("next cursor on the last page = %q", next)
	}
	// Field fidelity: the newest row is the match discrepancy with its IP.
	newest := entries[0]
	if newest.Action != "match" || newest.ActorID != "user-1" || newest.ActorIP != "203.0.113.6" ||
		newest.TraceID != "trace-44" || newest.TenantID != "t-a" ||
		newest.EntityType != "requisition" || newest.EntityID != "r-1" ||
		newest.Details != `{"number":"PR-000001"}` {
		t.Fatalf("audit row fidelity: %+v", newest)
	}
	if newest.RecordedAt.IsZero() || newest.OccurredAt.IsZero() {
		t.Fatalf("audit row timestamps: %+v", newest)
	}

	// Action filter: only the two match rows, newest first.
	entries, _, err = s.ListAudit(t.Context(), "t-a", AuditFilter{Action: "match", Limit: 10})
	if err != nil {
		t.Fatalf("ListAudit filtered: %v", err)
	}
	if !equalIDs([]string{entries[0].EventID, entries[1].EventID}, []string{"a6", "a5"}) {
		t.Fatalf("match filter = %v", eventIDsAudit(entries))
	}

	// Keyset pagination walks every row exactly once.
	walked := []string{}
	cursor := ""
	pages := 0
	for {
		page, nextCursor, err := s.ListAudit(t.Context(), "t-a", AuditFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListAudit page %d: %v", pages+1, err)
		}
		walked = append(walked, eventIDsAudit(page)...)
		pages++
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	if !equalIDs(walked, want) {
		t.Fatalf("walked pages = %v, want %v", walked, want)
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
}

func eventIDsAudit(entries []AuditEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.EventID)
	}
	return ids
}

// TestListAuditCrossTenantAndInvalid pins the absolute tenant scoping (the
// other tenant's audit rows are invisible) and the typed filter failures.
func TestListAuditCrossTenantAndInvalid(t *testing.T) {
	s := newStore(t)
	auditSeed(t, s)
	other, err := NewEvent("b1", "t-b", "pr.approved", testBase.Add(time.Hour),
		"user-b", "198.51.100.9", "trace-b", "requisition", "r-1", `{"number":"PR-000009"}`)
	if err != nil {
		t.Fatalf("NewEvent tenant B: %v", err)
	}
	if _, err := s.Apply(t.Context(), other); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	// Cross-tenant denial: tenant B sees none of tenant A's audit rows.
	entries, _, err := s.ListAudit(t.Context(), "t-b", AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAudit tenant B: %v", err)
	}
	if len(entries) != 1 || entries[0].TenantID != "t-b" {
		t.Fatalf("tenant B sees %v, want only its own row", eventIDsAudit(entries))
	}

	// Typed failures, the ListEvents convention.
	if _, _, err := s.ListAudit(t.Context(), "t-a", AuditFilter{Action: "decide"}); err != ErrActionInvalid {
		t.Fatalf("unknown action = %v, want ErrActionInvalid", err)
	}
	if _, _, err := s.ListAudit(t.Context(), "t-a", AuditFilter{EntityType: "requisition"}); err != ErrFilterInvalid {
		t.Fatalf("half filter = %v, want ErrFilterInvalid", err)
	}
	if _, _, err := s.ListAudit(t.Context(), "t-a", AuditFilter{Cursor: "junk"}); err != ErrCursorInvalid {
		t.Fatalf("bad cursor = %v, want ErrCursorInvalid", err)
	}
}

// TestExportAudit pins the export read: the newest MaxAuditExportRows rows
// of the tenant only, bounded columns rendered by AuditCSV (pinned in
// history_test.go), and tenant scoping.
func TestExportAudit(t *testing.T) {
	s := newStore(t)
	auditSeed(t, s)
	other, err := NewEvent("b1", "t-b", "pr.approved", testBase.Add(time.Hour),
		"user-b", "198.51.100.9", "trace-b", "requisition", "r-1", `{"number":"PR-000009"}`)
	if err != nil {
		t.Fatalf("NewEvent tenant B: %v", err)
	}
	if _, err := s.Apply(t.Context(), other); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	entries, err := s.ExportAudit(t.Context(), "t-a")
	if err != nil {
		t.Fatalf("ExportAudit: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("export rows = %d, want 6", len(entries))
	}
	if entries[0].EventID != "a6" || entries[0].Action != "match" {
		t.Fatalf("export order = %s, want a6 (the newest decision) first", entries[0].EventID)
	}
	if csv := string(AuditCSV(entries)); !strings.Contains(csv, "a6") || strings.Contains(csv, "PR-000001") {
		t.Fatalf("export csv missing newest row or leaking details")
	}

	// Tenant scoping: tenant B exports only its own single row.
	entries, err = s.ExportAudit(t.Context(), "t-b")
	if err != nil {
		t.Fatalf("ExportAudit tenant B: %v", err)
	}
	if len(entries) != 1 || entries[0].EventID != "b1" {
		t.Fatalf("tenant B export = %v, want [b1]", eventIDsAudit(entries))
	}
}

// TestApplyToleratesMissingFields pins decode tolerance: documents written
// without version/actor_id/trace_id/payload (pre-upgrade or hand-written
// rows, e.g. the pre-#42 seed shape) decode with zero values, not errors.
func TestApplyToleratesMissingFields(t *testing.T) {
	s := newStore(t)
	_, err := s.events.InsertOne(t.Context(), bson.M{
		"event_id":    "legacy-1",
		"tenant_id":   "t-a",
		"type":        "pr.submitted",
		"entity_type": "requisition",
		"entity_id":   "r-9",
		"occurred_at": testBase,
	})
	if err != nil {
		t.Fatalf("insert legacy doc: %v", err)
	}
	events, _, err := s.List(t.Context(), "t-a", ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List with legacy doc: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.Version != 0 || got.ActorID != "" || got.TraceID != "" || got.PayloadJSON != "" {
		t.Fatalf("missing fields did not decode to zero values: %+v", got)
	}
	if got.EventID != "legacy-1" || got.Type != "pr.submitted" {
		t.Fatalf("present fields lost: %+v", got)
	}
}

// TestEnsureIndexes pins the index set — names, key patterns, directions,
// uniqueness — so a rename or drift fails here, not in production.
func TestEnsureIndexes(t *testing.T) {
	s := newStore(t)

	type wantIndex struct {
		keys   bson.D
		unique bool
	}
	eventsWant := map[string]wantIndex{
		"uniq_event_id":          {bson.D{{Key: "event_id", Value: 1}}, true},
		"tenant_occurred":        {bson.D{{Key: "tenant_id", Value: 1}, {Key: "occurred_at", Value: -1}, {Key: "event_id", Value: -1}}, false},
		"tenant_entity_occurred": {bson.D{{Key: "tenant_id", Value: 1}, {Key: "entity_type", Value: 1}, {Key: "entity_id", Value: 1}, {Key: "occurred_at", Value: -1}, {Key: "event_id", Value: -1}}, false},
	}
	auditWant := map[string]wantIndex{
		"uniq_event_id":   {bson.D{{Key: "event_id", Value: 1}}, true},
		"tenant_occurred": {bson.D{{Key: "tenant_id", Value: 1}, {Key: "occurred_at", Value: -1}}, false},
	}

	assertIndexes := func(coll *mongo.Collection, want map[string]wantIndex) {
		t.Helper()
		specs, err := coll.Indexes().ListSpecifications(t.Context())
		if err != nil {
			t.Fatalf("list indexes: %v", err)
		}
		got := map[string]wantIndex{}
		for _, spec := range specs {
			if spec.Name == "_id_" {
				continue
			}
			unique := spec.Unique != nil && *spec.Unique
			got[spec.Name] = wantIndex{keys: bsonDFromRaw(t, spec.KeysDocument), unique: unique}
		}
		if len(got) != len(want) {
			t.Fatalf("indexes = %v, want exactly %d named (+_id_)", got, len(want))
		}
		for name, w := range want {
			g, ok := got[name]
			if !ok {
				t.Fatalf("missing index %q; have %v", name, got)
			}
			if !equalBsonD(g.keys, w.keys) || g.unique != w.unique {
				t.Fatalf("index %q = (%v, unique=%v), want (%v, unique=%v)",
					name, g.keys, g.unique, w.keys, w.unique)
			}
		}
	}
	assertIndexes(s.events, eventsWant)
	assertIndexes(s.audit, auditWant)
}

// bsonDFromRaw decodes a bson.Raw key document (e.g. an index
// specification) into bson.D for comparison.
func bsonDFromRaw(t *testing.T, raw bson.Raw) bson.D {
	t.Helper()
	var d bson.D
	if err := bson.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decode raw bson: %v", err)
	}
	return d
}

// equalBsonD compares two key documents element-wise. Numeric values are
// compared as int64 so int literals match the int32 values MongoDB
// round-trips.
func equalBsonD(a, b bson.D) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || !equalValues(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}

func equalValues(a, b any) bool {
	ai, aok := toInt64(a)
	bi, bok := toInt64(b)
	if aok && bok {
		return ai == bi
	}
	return a == b
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}
