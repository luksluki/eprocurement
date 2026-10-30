package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
)

const tenantID = "eeeeeeee-0000-0000-0000-000000000001"

// openTestDB applies the embedded migrations and opens a pool; skipped
// without TEST_DATABASE_URL (same harness shape as the approval tests).
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	migCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		t.Fatalf("open migration pool: %v", err)
	}
	if _, err := dbmigrate.Apply(t.Context(), migPool, "procurement", migrations.FS()); err != nil {
		migPool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	migPool.Close()

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	t.Cleanup(func() {
		_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`DELETE FROM procurement.outbox WHERE tenant_id = $1`, tenantID)
			return err
		})
	})
	return db
}

type samplePayload struct {
	Number   string `json:"number"`
	Decision string `json:"decision"`
}

// row mirrors the columns the publisher (#45) and the entity filters read.
type row struct {
	ID          string
	TenantID    string
	EventType   string
	Version     int32
	Payload     []byte
	ActorID     *string
	ActorIP     string
	TraceID     string
	EntityType  string
	EntityID    string
	OccurredAt  time.Time
	PublishedAt *time.Time
}

func loadRow(t *testing.T, db *pgxpool.Pool, id string) row {
	t.Helper()
	var r row
	err := tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT id, tenant_id::text, event_type, version, payload,
			       actor_id::text, actor_ip, trace_id, entity_type, entity_id::text,
			       occurred_at, published_at
			FROM procurement.outbox WHERE id = $1`, id).Scan(
			&r.ID, &r.TenantID, &r.EventType, &r.Version, &r.Payload,
			&r.ActorID, &r.ActorIP, &r.TraceID, &r.EntityType, &r.EntityID,
			&r.OccurredAt, &r.PublishedAt)
	})
	if err != nil {
		t.Fatalf("load outbox row: %v", err)
	}
	return r
}

// Happy path: AppendTx inside a tenant transaction writes every column —
// version defaults to 1, published_at stays NULL until #45 marks it, the
// payload lands as JSONB, and the tenant RLS context scoped the insert.
func TestAppendTxRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Microsecond)
	ev := Event{
		ID: "eeeeeeee-0000-0000-0000-0000000000aa", TenantID: tenantID,
		EventType: "pr.step_approved", Payload: samplePayload{Number: "PR-000001", Decision: "approve"},
		ActorID: "eeeeeeee-0000-0000-0000-0000000000bb", TraceID: "trace-31",
		ActorIP:    "203.0.113.7", // #44: the acting client's IP rides the row
		EntityType: "requisition", EntityID: "eeeeeeee-0000-0000-0000-0000000000cc",
		OccurredAt: at,
	}
	if err := tenant.WithTenant(ctx, db, tenantID, func(tx pgx.Tx) error {
		return AppendTx(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	r := loadRow(t, db, ev.ID)
	if r.TenantID != tenantID || r.EventType != "pr.step_approved" || r.Version != 1 ||
		r.ActorID == nil || *r.ActorID != ev.ActorID || r.TraceID != "trace-31" ||
		r.ActorIP != "203.0.113.7" ||
		r.EntityType != "requisition" || r.EntityID != ev.EntityID || r.PublishedAt != nil {
		t.Fatalf("row = %+v", r)
	}
	if !r.OccurredAt.Equal(at) {
		t.Fatalf("occurred_at = %v, want %v", r.OccurredAt, at)
	}
	var p samplePayload
	if err := json.Unmarshal(r.Payload, &p); err != nil || p != (samplePayload{Number: "PR-000001", Decision: "approve"}) {
		t.Fatalf("payload = %s (%v)", r.Payload, err)
	}

	// The publisher's read side carries actor_ip out with the row (#44) so
	// the §14.5 envelope stays column-assembled, no payload parsing. The
	// limit is generous on purpose: Pending returns every tenant's oldest
	// unpublished rows and the DB-backed suites run in parallel against one
	// test database (dev-note 045), so other suites' unmarked rows can sit
	// ahead of this one — the assertion matches the specific id, never a
	// position or a global count.
	pending, err := Pending(ctx, db, 1000)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	found := false
	for _, pe := range pending {
		if pe.ID == ev.ID {
			found = true
			if pe.ActorIP != "203.0.113.7" {
				t.Fatalf("Pending actor_ip = %q, want 203.0.113.7", pe.ActorIP)
			}
		}
	}
	if !found {
		t.Fatalf("appended row not returned by Pending")
	}
}

// Failure path: an event missing its identifying fields is rejected before
// any insert, and an error later in the same transaction rolls the appended
// row back with it — the atomicity the transactional outbox exists for.
func TestAppendTxGuardAndRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Missing fields fail fast (no database round-trip needed).
	for name, ev := range map[string]Event{
		"no tenant": {ID: "x", EventType: "pr.approved", EntityType: "requisition", EntityID: "e"},
		"no type":   {ID: "x", TenantID: tenantID, EntityType: "requisition", EntityID: "e"},
		"no entity": {ID: "x", TenantID: tenantID, EventType: "pr.approved"},
	} {
		if err := AppendTx(ctx, nil, ev); err == nil {
			t.Fatalf("%s: AppendTx succeeded, want error", name)
		}
	}

	// The row follows its transaction: a later failure leaves no row.
	sentinel := errors.New("boom")
	err := tenant.WithTenant(ctx, db, tenantID, func(tx pgx.Tx) error {
		if err := AppendTx(ctx, tx, Event{
			ID: "eeeeeeee-0000-0000-0000-0000000000dd", TenantID: tenantID,
			EventType: "pr.rejected", Payload: samplePayload{},
			EntityType: "requisition", EntityID: "eeeeeeee-0000-0000-0000-0000000000cc",
			OccurredAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return sentinel // simulates a guarded write failing after the append
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("tx err = %v, want the sentinel", err)
	}
	var n int
	if err := tenant.WithBypass(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM procurement.outbox WHERE id = $1`,
			"eeeeeeee-0000-0000-0000-0000000000dd").Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rolled-back row count = %d, want 0", n)
	}
}
