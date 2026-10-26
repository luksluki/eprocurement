package consumer

// Unit tests for the consumer's own concerns: envelope decode/validation
// (against the shared publisher.Envelope) and runner wiring. The retry
// ladder and topology declarations are tested where they live since the
// #46 merge — services/worker/topology — and are no longer duplicated here.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/services/worker/publisher"
	"github.com/luksluki/eprocurement/services/worker/topology"
)

func validEnvelope() publisher.Envelope {
	return publisher.Envelope{
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
	ev, err := Event(validEnvelope())
	if err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	if ev.EventID != "ev-47" || ev.PayloadJSON == "" {
		t.Fatalf("event = %+v", ev)
	}

	cases := map[string]func(*publisher.Envelope){
		"missing entity_type":  func(e *publisher.Envelope) { e.EntityType = "" },
		"missing entity_id":    func(e *publisher.Envelope) { e.EntityID = "" },
		"missing event_id":     func(e *publisher.Envelope) { e.EventID = " " },
		"missing tenant_id":    func(e *publisher.Envelope) { e.TenantID = "" },
		"missing type":         func(e *publisher.Envelope) { e.Type = "" },
		"zero occurred_at":     func(e *publisher.Envelope) { e.OccurredAt = time.Time{} },
		"invalid payload json": func(e *publisher.Envelope) { e.Payload = json.RawMessage(`{"no"`) },
		"empty payload":        func(e *publisher.Envelope) { e.Payload = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnvelope()
			mut(&env)
			if _, err := Event(env); !errors.Is(err, ErrEnvelopeInvalid) {
				t.Fatalf("Event() error = %v, want ErrEnvelopeInvalid", err)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	hist := topology.Default.Queues[0].Name
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
		Handlers: map[string]Handler{topology.Default.Queues[0].Name: nil}})
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
