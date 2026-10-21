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
