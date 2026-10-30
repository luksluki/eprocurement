// Read side of the transactional outbox for the publisher loop (#45; PRD
// §16 "Outbox publisher"). The reader lives with the table's owning service
// (procurement) while the loop itself runs in the worker — a conscious
// cross-service import inside the single Go module, recorded in
// docs/schema/procurement.md.
//
// Pending and MarkPublished read and write across tenants on purpose: the
// publisher drains every tenant's rows in one batch, which is why they run
// through the transaction-scoped app.rls_bypass arm (the same escape the auth
// flows use). This is the deliberate, documented inverse of the "every query
// is tenant-scoped" rule — the writer side (AppendTx inside WithTenant) keeps
// RLS enforced and is tested in outbox_test.go.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// PendingEvent is one unpublished outbox row as the publisher sees it. The
// columns map 1:1 onto the PRD §14.5 envelope fields so the publisher
// assembles it without parsing the payload.
type PendingEvent struct {
	ID         string // = event_id
	TenantID   string
	EventType  string
	Version    int32
	Payload    json.RawMessage
	ActorID    string // "" when the row's actor_id is NULL
	ActorIP    string // the acting client's IP (#44); "" when unknown
	TraceID    string
	EntityType string
	EntityID   string
	OccurredAt time.Time
	CreatedAt  time.Time
}

// Pending returns up to limit unpublished rows, oldest first (created_at,
// then id for a deterministic order), across ALL tenants via the RLS bypass
// arm. It never marks anything: a row stays pending until MarkPublished
// stamps it after a successful publish, so a crash between commit and
// publish — or between publish and mark — simply returns the row on the
// next poll (at-least-once; consumers dedupe on event_id, PRD §14.8).
func Pending(ctx context.Context, db *pgxpool.Pool, limit int) ([]PendingEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("outbox: Pending limit must be positive, got %d", limit)
	}
	events := []PendingEvent{}
	err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, event_type, version, payload,
			       coalesce(actor_id::text, ''), actor_ip, trace_id, entity_type, entity_id::text,
			       occurred_at, created_at
			FROM procurement.outbox
			WHERE published_at IS NULL
			ORDER BY created_at, id
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev PendingEvent
			if err := rows.Scan(&ev.ID, &ev.TenantID, &ev.EventType, &ev.Version,
				&ev.Payload, &ev.ActorID, &ev.ActorIP, &ev.TraceID, &ev.EntityType, &ev.EntityID,
				&ev.OccurredAt, &ev.CreatedAt); err != nil {
				return err
			}
			events = append(events, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("outbox: pending: %w", err)
	}
	return events, nil
}

// MarkPublished stamps published_at = now() on the given rows (any that are
// still unpublished — the guard makes re-marking a no-op instead of
// overwriting the first timestamp). Rows are never deleted: the outbox is
// append-only history. Empty ids is a no-op. A row that is already marked
// (a crash between publish and mark, replayed by the next poll) is simply
// skipped by the guard — reporting a row count back would only invite the
// caller to roll the transaction back, which would undo the marking.
func MarkPublished(ctx context.Context, db *pgxpool.Pool, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE procurement.outbox
			SET published_at = now()
			WHERE id = ANY($1::uuid[]) AND published_at IS NULL`, ids)
		return err
	})
	if err != nil {
		return fmt.Errorf("outbox: mark published: %w", err)
	}
	return nil
}
