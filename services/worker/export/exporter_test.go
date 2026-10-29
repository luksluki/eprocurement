package export

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/services/documents/objectstore"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
)

// minioEnv mirrors the documents testenv helper (an internal package the
// worker cannot import): TEST_S3_ENDPOINT gates the test; the credentials
// default to the compose values.
func minioEnv(t *testing.T) (endpoint, accessKey, secretKey, bucket string) {
	t.Helper()
	endpoint = os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set; skipping integration test")
	}
	if v := os.Getenv("TEST_S3_ACCESS_KEY"); v != "" {
		accessKey = v
	} else {
		accessKey = "eprocurement"
	}
	if v := os.Getenv("TEST_S3_SECRET_KEY"); v != "" {
		secretKey = v
	} else {
		secretKey = "eprocurement"
	}
	return endpoint, accessKey, secretKey, "attachments-test"
}

// Export-suite tenants (distinct from every other suite's fixtures).
const (
	xTenantA = "66666666-0049-0000-0000-0000000000aa"
	xTenantB = "66666666-0049-0000-0000-0000000000bb"
)

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
	return db
}

// seedPO inserts one purchase order (vendor + requisition + PO) and returns
// its number. The register is a read-only query, so plain SQL fixtures are
// enough — no domain flow needed.
func seedPO(t *testing.T, db *pgxpool.Pool, tenant, number string, issued *time.Time, status string) {
	t.Helper()
	ctx := context.Background()
	var vendorID, reqID string
	if err := db.QueryRow(ctx, `
		INSERT INTO procurement.vendors (tenant_id, code, name)
		VALUES ($1, $2, $3) RETURNING id::text`,
		tenant, "V-"+number, "Vendor "+number).Scan(&vendorID); err != nil {
		t.Fatalf("seed vendor %s: %v", number, err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO procurement.requisitions (tenant_id, number, requester_id, status, currency)
		VALUES ($1, $2, '66666666-0049-0000-0000-0000000000ff', 'approved', 'IDR')
		RETURNING id::text`,
		tenant, "PR-"+number).Scan(&reqID); err != nil {
		t.Fatalf("seed requisition %s: %v", number, err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO procurement.purchase_orders
			(tenant_id, number, requisition_id, vendor_id, status, currency, total_cents, issued_at)
		VALUES ($1, $2, $3, $4, $5, 'IDR', 1000, $6)`,
		tenant, number, reqID, vendorID, status, issued); err != nil {
		t.Fatalf("seed PO %s: %v", number, err)
	}
}

func cleanupExportFixtures(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM procurement.purchase_orders WHERE tenant_id::text IN ($1, $2)`,
		`DELETE FROM procurement.requisitions WHERE tenant_id::text IN ($1, $2)`,
		`DELETE FROM procurement.vendors WHERE tenant_id::text IN ($1, $2)`,
	} {
		if _, err := db.Exec(ctx, stmt, xTenantA, xTenantB); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}
}

// TestRegisterWindow pins the register query: the trailing 24 h half-open
// window anchored on issued_at, ORDER BY po_number, drafts excluded, and
// strict tenant scoping.
func TestRegisterWindow(t *testing.T) {
	db := openTestDB(t)
	cleanupExportFixtures(t, db)
	t.Cleanup(func() { cleanupExportFixtures(t, db) })

	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	in := func(d time.Duration) *time.Time { at := fire.Add(-d); return &at }
	seedPO(t, db, xTenantA, "PO-000003", in(30*time.Hour), "issued")             // before the window
	seedPO(t, db, xTenantA, "PO-000001", in(10*time.Hour), "issued")             // inside
	seedPO(t, db, xTenantA, "PO-000002", in(24*time.Hour-time.Second), "issued") // inside, near the edge
	seedPO(t, db, xTenantA, "PO-000004", nil, "draft")                           // never issued
	seedPO(t, db, xTenantA, "PO-000005", &fire, "issued")                        // exactly at fire: outside (half-open)
	seedPO(t, db, xTenantB, "PO-000001", in(5*time.Hour), "issued")              // another tenant

	register := NewRegister(db)
	ctx := t.Context()

	tenants, err := register.Tenants(ctx)
	if err != nil {
		t.Fatalf("Tenants: %v", err)
	}
	joined := strings.Join(tenants, ",")
	if !strings.Contains(joined, xTenantA) || !strings.Contains(joined, xTenantB) {
		t.Fatalf("Tenants = %v, want both export-suite tenants", tenants)
	}

	rows, err := register.PurchaseOrders(ctx, xTenantA, fire.Add(-24*time.Hour), fire)
	if err != nil {
		t.Fatalf("PurchaseOrders: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (window + no draft + half-open upper bound)", len(rows))
	}
	if rows[0].PONumber != "PO-000001" || rows[1].PONumber != "PO-000002" {
		t.Fatalf("order = [%s, %s], want [PO-000001, PO-000002] (ORDER BY po_number)",
			rows[0].PONumber, rows[1].PONumber)
	}
	for _, r := range rows {
		if r.VendorCode == "" || r.VendorName == "" || r.Currency != "IDR" || r.TotalCents != 1000 || r.Status != "issued" {
			t.Fatalf("row fidelity: %+v", r)
		}
	}

	// Tenant B's window holds only its own PO.
	rowsB, err := register.PurchaseOrders(ctx, xTenantB, fire.Add(-24*time.Hour), fire)
	if err != nil {
		t.Fatalf("PurchaseOrders tenant B: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].VendorCode != "V-PO-000001" {
		t.Fatalf("tenant B rows = %+v, want exactly its own PO", rowsB)
	}
}

// TestExporterStoresCSV runs the full export against real MinIO: the CSV
// lands under the pinned key with the pinned header, a re-run of the same
// fire overwrites the same key (per-date idempotency), and the object is
// readable with the stored content type.
func TestExporterStoresCSV(t *testing.T) {
	db := openTestDB(t)
	cleanupExportFixtures(t, db)
	t.Cleanup(func() { cleanupExportFixtures(t, db) })

	endpoint, accessKey, secretKey, bucket := minioEnv(t)
	objects, err := objectstore.New(endpoint, "", accessKey, secretKey, bucket, false)
	if err != nil {
		t.Fatalf("connect object store: %v", err)
	}
	if err := objects.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	at := fire.Add(-10 * time.Hour)
	seedPO(t, db, xTenantA, "PO-000001", &at, "issued")
	seedPO(t, db, xTenantA, "PO-000002", &at, "acknowledged")

	schedule := mustSchedule(t, "01:00", "UTC")
	exporter := NewExporter(NewRegister(db), objects, schedule)
	ctx := t.Context()

	rows, key, err := exporter.Export(ctx, xTenantA, fire)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}
	wantKey := "exports/" + xTenantA + "/2026-10-28/purchase-orders.csv"
	if key != wantKey {
		t.Fatalf("key = %s, want %s", key, wantKey)
	}
	info, err := objects.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.ContentType != CSVContentType {
		t.Fatalf("content type = %s, want %s", info.ContentType, CSVContentType)
	}
	reader, err := objects.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	defer reader.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		t.Fatalf("read object: %v", err)
	}
	body := buf.String()
	if !strings.HasPrefix(body, "po_number,vendor_code,vendor_name,currency,total_cents,status,issued_at\r\n") {
		t.Fatalf("stored CSV = %q", body)
	}
	if !strings.Contains(body, "PO-000001,V-PO-000001,Vendor PO-000001,IDR,1000,issued,") {
		t.Fatalf("stored CSV missing row 1: %q", body)
	}

	// Re-run of the same fire: the same key, overwritten (row count may
	// change with the data; the key must not).
	if _, key2, err := exporter.Export(ctx, xTenantA, fire); err != nil || key2 != wantKey {
		t.Fatalf("re-run Export = (%q, %v), want key %q", key2, err, wantKey)
	}
	if err := objects.Remove(ctx, wantKey); err != nil {
		t.Fatalf("cleanup object: %v", err)
	}
}

// TestExporterEmptyRegister: a tenant with no POs in the window exports a
// header-only CSV — success, not an error.
func TestExporterEmptyRegister(t *testing.T) {
	db := openTestDB(t)
	cleanupExportFixtures(t, db)
	t.Cleanup(func() { cleanupExportFixtures(t, db) })

	endpoint, accessKey, secretKey, bucket := minioEnv(t)
	objects, err := objectstore.New(endpoint, "", accessKey, secretKey, bucket, false)
	if err != nil {
		t.Fatalf("connect object store: %v", err)
	}
	if err := objects.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	fire := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	schedule := mustSchedule(t, "01:00", "UTC")
	exporter := NewExporter(NewRegister(db), objects, schedule)

	rows, _, err := exporter.Export(t.Context(), xTenantB, fire)
	if err != nil {
		t.Fatalf("Export empty register: %v", err)
	}
	if rows != 0 {
		t.Fatalf("rows = %d, want 0", rows)
	}
	key := ObjectKey(xTenantB, schedule, fire)
	defer func() { _ = objects.Remove(t.Context(), key) }()
	reader, err := objects.Read(t.Context(), key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	defer reader.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		t.Fatalf("read object: %v", err)
	}
	if got := buf.String(); got != "po_number,vendor_code,vendor_name,currency,total_cents,status,issued_at\r\n" {
		t.Fatalf("header-only CSV = %q", got)
	}
}
