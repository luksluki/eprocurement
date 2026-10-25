package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestNextRetry(t *testing.T) {
	cases := []struct {
		count  int32
		wantQ  string
		wantOK bool
	}{
		{-1, "", false}, // hostile header: treated as exhausted, not as tier 0
		{0, "q.retry.1", true},
		{1, "q.retry.2", true},
		{2, "q.retry.3", true},
		{3, "", false}, // budget exhausted (MaxRetries=3) → DLQ
		{7, "", false},
	}
	for _, tc := range cases {
		gotQ, gotOK := NextRetry("q", tc.count)
		if gotQ != tc.wantQ || gotOK != tc.wantOK {
			t.Fatalf("NextRetry(q, %d) = (%q, %v), want (%q, %v)", tc.count, gotQ, gotOK, tc.wantQ, tc.wantOK)
		}
	}
}

// TestRetryCountTolerant pins the tolerant header decode: absent → 0, any
// AMQP integer width decodes, unknown types fall back to 0 (first
// delivery) rather than dropping the message.
func TestRetryCountTolerant(t *testing.T) {
	cases := []struct {
		name string
		h    amqp.Table
		want int32
	}{
		{"nil table", nil, 0},
		{"absent", amqp.Table{"x-other": int32(1)}, 0},
		{"int8", amqp.Table{RetryCountHeader: int8(2)}, 2},
		{"int16", amqp.Table{RetryCountHeader: int16(2)}, 2},
		{"int32", amqp.Table{RetryCountHeader: int32(2)}, 2},
		{"int64", amqp.Table{RetryCountHeader: int64(2)}, 2},
		{"int", amqp.Table{RetryCountHeader: int(2)}, 2},
		{"string is not a count", amqp.Table{RetryCountHeader: "two"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryCount(tc.h); got != tc.want {
				t.Fatalf("RetryCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRetryTTLsPinned(t *testing.T) {
	want := []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}
	if len(DefaultRetryTTLs) != len(want) {
		t.Fatalf("DefaultRetryTTLs = %v, want %v", DefaultRetryTTLs, want)
	}
	for i := range want {
		if DefaultRetryTTLs[i] != want[i] {
			t.Fatalf("DefaultRetryTTLs[%d] = %v, want %v", i, DefaultRetryTTLs[i], want[i])
		}
	}
}

// TestQueueArgsPinned pins the queue arguments argument-for-argument — they
// must stay identical to the #45/#46 declarations or a re-declare on an
// existing broker raises PRECONDITION_FAILED.
func TestQueueArgsPinned(t *testing.T) {
	if got := mainQueueArgs("procurement.dlx"); len(got) != 1 ||
		got["x-dead-letter-exchange"] != "procurement.dlx" {
		t.Fatalf("mainQueueArgs = %v", got)
	}
	got := retryQueueArgs("documents.history", 5*time.Second)
	if len(got) != 3 ||
		got["x-message-ttl"] != int64(5000) ||
		got["x-dead-letter-exchange"] != "" ||
		got["x-dead-letter-routing-key"] != "documents.history" {
		t.Fatalf("retryQueueArgs = %v", got)
	}
}

// TestTopologyDefaultPinned pins the production topology: names, bindings,
// and retry ladder of the two #47 queues.
func TestTopologyDefaultPinned(t *testing.T) {
	d := Default
	if d.EventsExchange != "procurement.events" || d.DLXExchange != "procurement.dlx" ||
		d.DLQQueue != "procurement.dlq" {
		t.Fatalf("exchange/dlq names = %v/%v/%v", d.EventsExchange, d.DLXExchange, d.DLQQueue)
	}
	if len(d.Queues) != 2 {
		t.Fatalf("queues = %d, want 2", len(d.Queues))
	}
	hist, notif := d.Queues[0], d.Queues[1]
	if hist.Name != "documents.history" || len(hist.Bindings) != 1 || hist.Bindings[0] != "#" {
		t.Fatalf("history spec = %+v", hist)
	}
	if notif.Name != "notifications" {
		t.Fatalf("notifications name = %q", notif.Name)
	}
	wantKeys := []string{
		"pr.submitted", "pr.approved", "pr.rejected",
		"po.issued", "po.acknowledged", "invoice.discrepancy",
	}
	if len(notif.Bindings) != len(wantKeys) {
		t.Fatalf("notifications bindings = %v, want %v", notif.Bindings, wantKeys)
	}
	for i, k := range wantKeys {
		if notif.Bindings[i] != k {
			t.Fatalf("notifications binding[%d] = %q, want %q", i, notif.Bindings[i], k)
		}
	}
	if len(d.RetryTTLs) != MaxRetries {
		t.Fatalf("RetryTTLs = %v", d.RetryTTLs)
	}
	wantQueues := []string{
		"documents.history",
		"documents.history.retry.1", "documents.history.retry.2", "documents.history.retry.3",
		"notifications",
		"notifications.retry.1", "notifications.retry.2", "notifications.retry.3",
		"procurement.dlq",
	}
	got := d.QueueNames()
	if len(got) != len(wantQueues) {
		t.Fatalf("QueueNames = %v, want %v", got, wantQueues)
	}
	for i, name := range wantQueues {
		if got[i] != name {
			t.Fatalf("QueueNames[%d] = %q, want %q", i, got[i], name)
		}
	}
}

// TestTopologySuffixedIsolated pins the test-variant derivation: every name
// (including derived retry queues) carries the suffix, TTLs are copied, and
// mutating the copy does not touch the original.
func TestTopologySuffixedIsolated(t *testing.T) {
	s := Default.Suffixed("-it")
	if s.EventsExchange != "procurement.events-it" || s.DLQQueue != "procurement.dlq-it" {
		t.Fatalf("suffixed names = %v/%v", s.EventsExchange, s.DLQQueue)
	}
	if s.Queues[0].Name != "documents.history-it" ||
		s.QueueNames()[1] != "documents.history-it.retry.1" {
		t.Fatalf("suffixed queue names = %v", s.QueueNames())
	}
	s.RetryTTLs[0] = time.Millisecond
	if Default.RetryTTLs[0] != 5*time.Second {
		t.Fatalf("mutating the copy changed Default: %v", Default.RetryTTLs)
	}
}

// TestDeclareRejectsWrongTTLCount pins the guard: a topology whose retry
// ladder does not have exactly MaxRetries tiers is a wiring bug.
func TestDeclareRejectsWrongTTLCount(t *testing.T) {
	topo := Default
	topo.RetryTTLs = []time.Duration{time.Second}
	if err := topo.Declare(nil); err == nil {
		t.Fatal("Declare with 1 TTL succeeded, want error")
	}
}

func validEnvelope() Envelope {
	return Envelope{
		EventID: "ev-47", TenantID: "t-47", Type: "pr.approved", Version: 1,
		EntityType: "requisition", EntityID: "req-1",
		OccurredAt: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
		ActorID:    "user-9", TraceID: "trace-47",
		Payload: json.RawMessage(`{"requester_id":"user-1","decision":"approve"}`),
	}
}

func TestDecodeEnvelope(t *testing.T) {
	env, err := Decode([]byte(`{
		"event_id":"ev-47","tenant_id":"t-47","type":"pr.approved","version":1,
		"entity_type":"requisition","entity_id":"req-1",
		"occurred_at":"2026-10-25T08:00:00Z","actor_id":"user-9","trace_id":"trace-47",
		"payload":{"requester_id":"user-1"}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.EventID != "ev-47" || env.EntityType != "requisition" ||
		env.EntityID != "req-1" || env.Type != "pr.approved" ||
		string(env.Payload) != `{"requester_id":"user-1"}` {
		t.Fatalf("decoded envelope = %+v", env)
	}

	for name, body := range map[string]string{
		"not json":         `{"event_id":`,
		"empty body":       ``,
		"json array":       `[1,2,3]`,
		"trailing garbage": `{"event_id":"ev"} trailing`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(body)); !errors.Is(err, ErrEnvelopeInvalid) {
				t.Fatalf("Decode(%s) error = %v, want ErrEnvelopeInvalid", body, err)
			}
		})
	}
}

// TestEnvelopeEventValidation pins the permanent-invalid boundary at the
// consumer: an envelope that cannot produce a valid history.Event — the
// projection both stores apply — is ErrEnvelopeInvalid (dead-letter, no
// retry), while the full envelope passes.
func TestEnvelopeEventValidation(t *testing.T) {
	ev, err := validEnvelope().Event()
	if err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	if ev.EventID != "ev-47" || ev.PayloadJSON == "" {
		t.Fatalf("event = %+v", ev)
	}

	cases := map[string]func(*Envelope){
		"missing entity_type":  func(e *Envelope) { e.EntityType = "" },
		"missing entity_id":    func(e *Envelope) { e.EntityID = "" },
		"missing event_id":     func(e *Envelope) { e.EventID = " " },
		"missing tenant_id":    func(e *Envelope) { e.TenantID = "" },
		"missing type":         func(e *Envelope) { e.Type = "" },
		"zero occurred_at":     func(e *Envelope) { e.OccurredAt = time.Time{} },
		"invalid payload json": func(e *Envelope) { e.Payload = json.RawMessage(`{"no"`) },
		"empty payload":        func(e *Envelope) { e.Payload = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnvelope()
			mut(&env)
			if _, err := env.Event(); !errors.Is(err, ErrEnvelopeInvalid) {
				t.Fatalf("Event() error = %v, want ErrEnvelopeInvalid", err)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	hist := Default.Queues[0].Name
	if _, err := New(Config{Handlers: map[string]Handler{hist: nil}}); err == nil {
		t.Fatal("New without URL succeeded, want error")
	}
	if _, err := New(Config{URL: "amqp://x"}); err == nil {
		t.Fatal("New without handlers succeeded, want error")
	}
	if _, err := New(Config{URL: "amqp://x", Handlers: map[string]Handler{
		"not-declared": nil,
	}}); err == nil {
		t.Fatal("New with a handler for an undeclared queue succeeded, want error")
	}
	r, err := New(Config{URL: "amqp://x", Handlers: map[string]Handler{hist: nil}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.prefetch != 10 || r.deliveryTimeout != 10*time.Second ||
		r.topo.EventsExchange != "procurement.events" || r.log == nil {
		t.Fatalf("defaults not filled: prefetch=%d timeout=%v topo=%+v log=%v",
			r.prefetch, r.deliveryTimeout, r.topo, r.log)
	}
}

// TestRunReturnsNilWhenCancelledWithoutBroker pins the shutdown half of
// PRD §14.8 "worker down": a broker that never answers must not crash-loop
// the process out of Run — SIGINT/SIGTERM (ctx cancel) ends it with nil.
func TestRunReturnsNilWhenCancelledWithoutBroker(t *testing.T) {
	// The handler is never invoked — the broker is unreachable; the map
	// only satisfies New's wiring check.
	r, err := New(Config{URL: "amqp://127.0.0.1:1/",
		Handlers: map[string]Handler{Default.Queues[0].Name: nil}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
