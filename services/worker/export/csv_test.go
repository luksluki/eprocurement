package export

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestWriteCSV pins the register format: the pinned header, RFC 4180
// quoting (encoding/csv), RFC 3339 UTC timestamps, and the header-only
// file for an empty register — a valid "no POs today" export.
func TestWriteCSV(t *testing.T) {
	issued := time.Date(2026, 10, 26, 4, 30, 0, 0, time.UTC)
	rows := []PurchaseOrderRow{
		{PONumber: "PO-000001", VendorCode: "V-01", VendorName: "PT Contoh", Currency: "IDR", TotalCents: 15000000, Status: "issued", IssuedAt: issued},
		{PONumber: "PO-000002", VendorCode: "V-02", VendorName: "CV Sinar, Jaya", Currency: "USD", TotalCents: -0, Status: "acknowledged", IssuedAt: issued},
		{PONumber: "PO-000003", VendorCode: "V-03", VendorName: `PT "Kutip"`, Currency: "IDR", TotalCents: 999, Status: "closed", IssuedAt: issued},
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	got := buf.String()

	wantHeader := "po_number,vendor_code,vendor_name,currency,total_cents,status,issued_at\r\n"
	if !strings.HasPrefix(got, wantHeader) {
		t.Fatalf("header = %q, want %q", got[:len(wantHeader)], wantHeader)
	}
	// A comma in the vendor name must be quoted (RFC 4180)…
	if !strings.Contains(got, "\"CV Sinar, Jaya\"") {
		t.Fatalf("comma field not quoted: %q", got)
	}
	// …as must embedded double quotes (doubled per RFC 4180).
	if !strings.Contains(got, "\"PT \"\"Kutip\"\"\"") {
		t.Fatalf("quote field not escaped: %q", got)
	}
	if !strings.Contains(got, ",15000000,issued,2026-10-26T04:30:00Z\r\n") {
		t.Fatalf("plain row malformed: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n") {
		t.Fatal("file must end with CRLF")
	}

	var empty bytes.Buffer
	if err := WriteCSV(&empty, nil); err != nil {
		t.Fatalf("WriteCSV empty: %v", err)
	}
	if empty.String() != wantHeader {
		t.Fatalf("empty register = %q, want header only", empty.String())
	}
}

// TestColumnsPin guards the maintainer-pinned column order (2026-10-27).
func TestColumnsPin(t *testing.T) {
	want := []string{"po_number", "vendor_code", "vendor_name", "currency", "total_cents", "status", "issued_at"}
	if len(Columns) != len(want) {
		t.Fatalf("Columns = %v, want %v", Columns, want)
	}
	for i := range want {
		if Columns[i] != want[i] {
			t.Fatalf("Columns[%d] = %s, want %s", i, Columns[i], want[i])
		}
	}
}

// TestObjectKey pins the export key layout: exports/{tenant}/{date}/
// purchase-orders.csv with the date anchored in the schedule's zone.
func TestObjectKey(t *testing.T) {
	jakarta := mustSchedule(t, "01:00", "Asia/Jakarta")
	// 2026-10-27 18:00 UTC == 2026-10-28 01:00 Jakarta.
	fire := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	if got, want := ObjectKey("t-1", jakarta, fire), "exports/t-1/2026-10-28/purchase-orders.csv"; got != want {
		t.Fatalf("ObjectKey = %s, want %s", got, want)
	}
	utc := mustSchedule(t, "01:00", "UTC")
	if got, want := ObjectKey("t-1", utc, fire), "exports/t-1/2026-10-27/purchase-orders.csv"; got != want {
		t.Fatalf("ObjectKey = %s, want %s", got, want)
	}
}
