package export

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// ObjectStore is the object-store write side the exporter needs — the
// documents MinIO adapter's Put (#49); the bucket is the single S3_BUCKET
// both the worker (writer) and documents (presigner) share, so a presigned
// GET can always resolve what the writer stored.
type ObjectStore interface {
	Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error
}

// Register reads the PO register out of procurement. Both queries span
// tenants on purpose (the publisher's arm): the worker enumerates every
// tenant and exports each one's slice, so they run through the
// transaction-scoped app.rls_bypass (tenant.WithBypass, the outbox.Pending
// precedent — dev-note 045), never the raw superuser pool. The per-tenant
// query itself is still tenant-scoped by its WHERE clause.
type Register struct {
	db *pgxpool.Pool
}

// NewRegister binds the register to the worker's PostgreSQL pool.
func NewRegister(db *pgxpool.Pool) *Register { return &Register{db: db} }

// Tenants lists every tenant that has purchase orders, ordered by id for
// deterministic logs (SELECT DISTINCT tenant_id — maintainer decision
// 2026-10-27: no app-schema tenants table exists).
func (r *Register) Tenants(ctx context.Context) ([]string, error) {
	tenants := []string{}
	err := tenant.WithBypass(ctx, r.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT tenant_id::text
			FROM procurement.purchase_orders
			ORDER BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			tenants = append(tenants, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("export: tenants: %w", err)
	}
	return tenants, nil
}

// PurchaseOrders returns the tenant's register for the half-open window
// [from, to), anchored on issued_at and ordered by po_number (the pinned
// CSV order). Draft POs (issued_at NULL) never appear.
func (r *Register) PurchaseOrders(ctx context.Context, tenantID string, from, to time.Time) ([]PurchaseOrderRow, error) {
	rows := []PurchaseOrderRow{}
	err := tenant.WithBypass(ctx, r.db, func(tx pgx.Tx) error {
		result, err := tx.Query(ctx, `
			SELECT po.number, v.code, v.name, po.currency, po.total_cents, po.status, po.issued_at
			FROM procurement.purchase_orders po
			JOIN procurement.vendors v ON v.id = po.vendor_id
			WHERE po.tenant_id = $1
			  AND po.issued_at >= $2
			  AND po.issued_at < $3
			ORDER BY po.number`, tenantID, from, to)
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			var row PurchaseOrderRow
			if err := result.Scan(&row.PONumber, &row.VendorCode, &row.VendorName,
				&row.Currency, &row.TotalCents, &row.Status, &row.IssuedAt); err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return result.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("export: purchase orders: %w", err)
	}
	return rows, nil
}

// Exporter produces one tenant's CSV per fire and stores it in MinIO.
type Exporter struct {
	register *Register
	objects  ObjectStore
	schedule *Daily // anchors the object key's date in EXPORT_TZ
}

// NewExporter binds the register, the object store, and the schedule.
func NewExporter(register *Register, objects ObjectStore, schedule *Daily) *Exporter {
	return &Exporter{register: register, objects: objects, schedule: schedule}
}

// ObjectKey is the pinned key: exports/{tenant_id}/{yyyy-mm-dd}/
// purchase-orders.csv with the date from the fire time in EXPORT_TZ. The
// same key per date makes a re-run overwrite idempotently.
func ObjectKey(tenantID string, schedule *Daily, fireTime time.Time) string {
	return fmt.Sprintf("exports/%s/%s/purchase-orders.csv", tenantID, schedule.SlotDate(fireTime))
}

// Export runs the tenant's register for the trailing 24 h before fireTime
// (half-open [fireTime-24h, fireTime)) and stores the CSV. It returns the
// row count and the object key. A failure leaves no object — the caller
// records the failed run with the error message (#53's error display).
func (e *Exporter) Export(ctx context.Context, tenantID string, fireTime time.Time) (int, string, error) {
	from := fireTime.Add(-24 * time.Hour)
	rows, err := e.register.PurchaseOrders(ctx, tenantID, from, fireTime)
	if err != nil {
		return 0, "", err
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows); err != nil {
		return 0, "", fmt.Errorf("export: csv encode: %w", err)
	}
	key := ObjectKey(tenantID, e.schedule, fireTime)
	if err := e.objects.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), CSVContentType); err != nil {
		return 0, "", fmt.Errorf("export: store %s: %w", key, err)
	}
	return len(rows), key, nil
}
