// Package export writes the manual export request (#53, FR-10's manual
// half): exactly one procurement.outbox row carrying the §14.6 catalog's
// export.requested event, committed alone in its own tenant transaction.
// There is no business state to change with it — the request IS the event
// (ADR-0001: procurement is the single outbox writer; the worker that runs
// the export must never write it). The worker's exports consumer binds the
// same routing key and owns the run.
package export

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/outbox"
)

// EventRequested is the §14.6 catalog name of the trigger event.
const EventRequested = "export.requested"

// RequestedPayload is the export.requested payload: who asked for the run.
// The worker anchors the export window on the event's occurred_at instead,
// so the payload stays minimal attribution.
type RequestedPayload struct {
	RequestedBy string `json:"requested_by"`
}

// Store writes export.requested outbox rows.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Request appends one export.requested event to procurement.outbox inside a
// tenant transaction and returns its id — the caller's correlation handle
// (the gateway answers 202 with it). The event id doubles as the entity id:
// the "export" aggregate instance is the request itself, so one identity
// names both. ActorIP/TraceID come from the gateway-propagated context
// (the outbox row pattern of every decision write, e.g. purchaseorder).
func (s *Store) Request(ctx context.Context, tenantID, actorID, actorIP, traceID string, occurredAt time.Time) (string, error) {
	eventID := uuid.NewString()
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return outbox.AppendTx(ctx, tx, outbox.Event{
			ID:         eventID,
			TenantID:   tenantID,
			EventType:  EventRequested,
			Version:    1,
			Payload:    RequestedPayload{RequestedBy: actorID},
			ActorID:    actorID,
			ActorIP:    actorIP,
			TraceID:    traceID,
			EntityType: "export",
			EntityID:   eventID,
			OccurredAt: occurredAt,
		})
	})
	if err != nil {
		return "", fmt.Errorf("export: request: %w", err)
	}
	return eventID, nil
}
