// Package vendor implements the vendor master-data domain: validation, the
// per-tenant uniqueness rule, and the soft-delete lifecycle. All storage goes
// through tenant.WithTenant, so PostgreSQL RLS scopes every statement.
package vendor

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Vendor is the aggregate root. Status is "active" or "deactivated"; a
// deactivated vendor is soft-deleted: readable, immutable, and unusable for
// new purchase orders.
type Vendor struct {
	ID           string
	TenantID     string
	Code         string
	Name         string
	ContactEmail string
	TaxID        string
	Status       string
	CreatedAt    time.Time
	CreatedBy    string
	UpdatedAt    time.Time
	UpdatedBy    string
}

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged.
var (
	ErrCodeInvalid   = errors.New("vendor_code_invalid")
	ErrNameInvalid   = errors.New("vendor_name_invalid")
	ErrEmailInvalid  = errors.New("vendor_email_invalid")
	ErrIDInvalid     = errors.New("vendor_id_invalid")
	ErrDuplicateCode = errors.New("vendor_duplicate_code")
	ErrNotFound      = errors.New("vendor_not_found")
	ErrDeactivated   = errors.New("vendor_deactivated")
	ErrStatusInvalid = errors.New("vendor_status_invalid")
	ErrPageInvalid   = errors.New("vendor_page_invalid")
)

var (
	codePattern  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{1,31}$`)
	emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// Normalize uppercases the vendor code (canonical form for uniqueness).
func Normalize(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// ValidateCreate checks the create payload fields.
func ValidateCreate(code, name, email string) error {
	if !codePattern.MatchString(code) {
		return ErrCodeInvalid
	}
	name = strings.TrimSpace(name)
	if len(name) < 2 || len(name) > 200 {
		return ErrNameInvalid
	}
	if email != "" && !emailPattern.MatchString(email) {
		return ErrEmailInvalid
	}
	return nil
}

// ValidateUpdate checks the mutable fields (empty keeps the stored value).
func ValidateUpdate(name, email string) error {
	if name != "" && (len(strings.TrimSpace(name)) < 2 || len(strings.TrimSpace(name)) > 200) {
		return ErrNameInvalid
	}
	if email != "" && !emailPattern.MatchString(email) {
		return ErrEmailInvalid
	}
	return nil
}

// Vendor lifecycle statuses.
const (
	StatusActive      = "active"
	StatusDeactivated = "deactivated"
)

// List bounds for pagination.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
	MaxPage         = 1000000
)

// ListFilter is the vendor-list query: a free-text match (code prefix or
// name substring), an optional status, and server-side pagination. Zero
// values fall back to the defaults; ordering is fixed by code ascending.
type ListFilter struct {
	Query    string
	Status   string
	Page     int32
	PageSize int32
}

// ValidateList normalizes and validates the filter: status must be a known
// value (or empty), page/page_size are clamped into range, and the query is
// trimmed and uppercased (codes are stored uppercase) with LIKE wildcards
// escaped so user input matches literally.
func ValidateList(f ListFilter) (ListFilter, error) {
	f.Status = strings.TrimSpace(f.Status)
	if f.Status != "" && f.Status != StatusActive && f.Status != StatusDeactivated {
		return f, ErrStatusInvalid
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Page > MaxPage {
		return f, ErrPageInvalid
	}
	if f.PageSize < 1 {
		f.PageSize = DefaultPageSize
	}
	if f.PageSize > MaxPageSize {
		f.PageSize = MaxPageSize
	}
	f.Query = escapeLike(strings.ToUpper(strings.TrimSpace(f.Query)))
	return f, nil
}

// escapeLike neutralizes LIKE/ILIKE wildcards so a query containing %, _,
// or \ matches those literal characters.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
