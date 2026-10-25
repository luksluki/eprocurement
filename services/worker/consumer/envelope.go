package consumer

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/luksluki/eprocurement/services/documents/history"
)

// ErrEnvelopeInvalid is the typed error for deliveries that can never
// succeed: a body that does not decode, or an envelope that fails
// validation. The runner dead-letters those straight to the DLQ without
// retry (they would fail forever — PRD §14.5 "after max attempts → DLQ"
// shortened to zero attempts for permanent failures).
var ErrEnvelopeInvalid = errors.New("consumer_envelope_invalid")

// Envelope is the PRD §14.5 event envelope as published by the outbox
// publisher (#45), extended with entity_type/entity_id — the outbox columns
// already exist (#31) and the history projection requires them (#42; PRD
// §14.5 simply did not enumerate them — maintainer-approved in issue #47).
// The field names and order match publisher.Envelope one-to-one; this copy
// exists because #47 builds self-contained against develop (PRs #94/#95
// unmerged) — when they merge, the swap to the shared type is mechanical.
type Envelope struct {
	EventID    string          `json:"event_id"`
	TenantID   string          `json:"tenant_id"`
	Type       string          `json:"type"`
	Version    int32           `json:"version"`
	EntityType string          `json:"entity_type"`
	EntityID   string          `json:"entity_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorID    string          `json:"actor_id"`
	TraceID    string          `json:"trace_id"`
	Payload    json.RawMessage `json:"payload"`
}

// Decode parses one delivery body into an Envelope. Any decode failure is
// ErrEnvelopeInvalid — the message is permanently broken, not transient.
func Decode(body []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("%w: decode body: %v", ErrEnvelopeInvalid, err)
	}
	return env, nil
}

// Event validates the envelope and maps it onto the history projection's
// Event (the shape both stores' Apply take). history.NewEvent normalizes
// (trims, defaults the version) and validates in one step, so the same
// checks the stores re-apply defensively decide the delivery's fate here.
func (e Envelope) Event() (history.Event, error) {
	ev, err := history.NewEvent(e.EventID, e.TenantID, e.Type, e.OccurredAt,
		e.ActorID, e.TraceID, e.EntityType, e.EntityID, string(e.Payload))
	if err != nil {
		return history.Event{}, fmt.Errorf("%w: %v", ErrEnvelopeInvalid, err)
	}
	return ev, nil
}

// isPermanent reports whether a handler error can never succeed on retry —
// the history validation sentinels (both stores validate through them).
// Everything else (Mongo down, network blip) is transient and takes the
// retry ladder.
func isPermanent(err error) bool {
	return errors.Is(err, history.ErrEventIDInvalid) ||
		errors.Is(err, history.ErrEventIncomplete) ||
		errors.Is(err, history.ErrPayloadInvalid)
}
