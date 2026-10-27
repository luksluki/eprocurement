package export

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"
)

// Columns is the pinned CSV header (maintainer decision 2026-10-27): the PO
// register per tenant for the trailing 24-hour window.
var Columns = []string{
	"po_number",
	"vendor_code",
	"vendor_name",
	"currency",
	"total_cents",
	"status",
	"issued_at",
}

// CSVContentType is stored alongside the object; UTF-8 is declared because
// vendor names may carry non-ASCII characters.
const CSVContentType = "text/csv; charset=utf-8"

// PurchaseOrderRow is one register line as the exporter reads it from
// procurement and writes it to the CSV.
type PurchaseOrderRow struct {
	PONumber   string
	VendorCode string
	VendorName string
	Currency   string
	TotalCents int64
	Status     string
	IssuedAt   time.Time
}

// WriteCSV writes the header plus one line per row, RFC 4180 (encoding/csv
// with UseCRLF: CRLF terminators, minimal quoting of separators/quotes/
// newlines) in UTF-8. issued_at is RFC 3339 UTC. An empty register yields a
// header-only file — a valid export of "no POs today", not an error.
func WriteCSV(w io.Writer, rows []PurchaseOrderRow) error {
	cw := csv.NewWriter(w)
	cw.UseCRLF = true // RFC 4180 record separators
	if err := cw.Write(Columns); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			r.PONumber,
			r.VendorCode,
			r.VendorName,
			r.Currency,
			strconv.FormatInt(r.TotalCents, 10),
			r.Status,
			r.IssuedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
