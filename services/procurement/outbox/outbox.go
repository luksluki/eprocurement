// Package outbox writes transactional-outbox rows: AppendTx inserts events
// inside the caller's transaction, so state changes and their events commit
// atomically (PRD §14.3; ADR-0001 — no dual writes). The publisher loop that
// drains procurement.outbox to RabbitMQ is #45.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event is one procurement.outbox row. ID is the event_id consumers dedupe
// on (PRD §14.5 envelope); callers generate it before writing so replays can
// be recognized without a database round-trip. Version is the payload schema
// version, starting at 1. EntityType/EntityID name the aggregate the event is
// about — the filter keys of GET /api/v1/events?entityType=&entityId=
// (PRD §15) and the timeline projection (#43); they are a conscious extension
// beyond the §13 summary and the §14.5 envelope, documented in
// docs/schema/procurement.md.
type Event struct {
	ID         string
	TenantID   string
	EventType  string
	Version    int32
	Payload    any // marshalled to the jsonb column
	ActorID    string
	TraceID    string
	EntityType string
	EntityID   string
	OccurredAt time.Time
}

// AppendTx inserts ev into procurement.outbox inside tx. It never commits:
// pair it with the guarded state writes of the same transaction — either
// both land or neither does. The publisher (#45) marks published_at later;
// rows are never updated otherwise and never deleted.
func AppendTx(ctx context.Context, tx pgx.Tx, ev Event) error {
	if ev.TenantID == "" || ev.EventType == "" || ev.EntityType == "" || ev.EntityID == "" {
		return fmt.Errorf("outbox: event requires tenant, type, and entity fields")
	}
	if ev.Version <= 0 {
		ev.Version = 1
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO procurement.outbox
			(id, tenant_id, event_type, version, payload, actor_id, trace_id,
			 entity_type, entity_id, occurred_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, $7, $8, $9, $10)`,
		ev.ID, ev.TenantID, ev.EventType, ev.Version, payload,
		ev.ActorID, ev.TraceID, ev.EntityType, ev.EntityID, ev.OccurredAt)
	if err != nil {
		return fmt.Errorf("outbox: append: %w", err)
	}
	return nil
}
