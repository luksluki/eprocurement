// Package attachment implements the attachment domain: declaration
// validation (mime allowlist, size limit, sha256 checksum), the
// pending→ready lifecycle, metadata storage in MongoDB document_meta, and
// the object-storage port implemented by the MinIO adapter. Attachment
// bytes never pass through this service — clients upload to and download
// from presigned URLs.
package attachment

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Statuses of the attachment lifecycle. A pending attachment has metadata
// and a presigned upload URL but no verified object yet; ready means the
// object was verified (size, content type, checksum).
const (
	StatusPending = "pending"
	StatusReady   = "ready"
)

// EntityTypeVendor is the only owner kind wired so far; requisitions and
// invoices follow the same shape in later issues.
const EntityTypeVendor = "vendor"

// Limits bounds the declaration at create time.
type Limits struct {
	MaxSizeBytes int64
}

// AllowedMimes is the declaration allowlist: contracts and tax documents
// (PDF, images, Office documents).
var AllowedMimes = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
	"image/webp":      true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       true,
}

// Machine-readable failure codes; the gRPC API and the gateway carry them
// through to clients unchanged.
var (
	ErrEntityInvalid    = errors.New("attachment_entity_invalid")
	ErrFilenameInvalid  = errors.New("attachment_filename_invalid")
	ErrMimeInvalid      = errors.New("attachment_mime_invalid")
	ErrTooLarge         = errors.New("attachment_too_large")
	ErrChecksumInvalid  = errors.New("attachment_checksum_invalid")
	ErrIDInvalid        = errors.New("attachment_id_invalid")
	ErrNotFound         = errors.New("attachment_not_found")
	ErrNotReady         = errors.New("attachment_not_ready")
	ErrObjectMissing    = errors.New("attachment_object_missing")
	ErrSizeMismatch     = errors.New("attachment_size_mismatch")
	ErrMimeMismatch     = errors.New("attachment_mime_mismatch")
	ErrChecksumMismatch = errors.New("attachment_checksum_mismatch")
)

const maxFilenameLen = 255

var (
	// idPattern accepts uuid-shaped ids: they end up inside object keys, so
	// path-safe characters only.
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	checksumPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Attachment is the aggregate root; one row of MongoDB document_meta.
type Attachment struct {
	ID             string
	TenantID       string
	EntityType     string
	EntityID       string
	Filename       string
	MimeType       string
	SizeBytes      int64
	ChecksumSHA256 string
	Status         string
	ObjectKey      string
	CreatedBy      string
	CreatedAt      time.Time
	UploadedAt     time.Time // zero while pending
}

// ObjectKey is the PRD §13 layout: attachments/{tenant}/{entity}/{uuid}.
func ObjectKey(tenantID, entityType, id string) string {
	return fmt.Sprintf("attachments/%s/%s/%s", tenantID, entityType, id)
}

// NormalizeCanonical lowercases mime type and checksum hex (canonical forms).
func NormalizeCanonical(mime, checksum string) (string, string) {
	return strings.ToLower(strings.TrimSpace(mime)), strings.ToLower(strings.TrimSpace(checksum))
}

// ValidateCreate checks the upload declaration. entityType and entityID are
// validated strictly because they become path segments of the object key.
func ValidateCreate(limits Limits, entityType, entityID, filename, mime string, size int64, checksum string) error {
	if entityType != EntityTypeVendor {
		return ErrEntityInvalid
	}
	if !idPattern.MatchString(entityID) {
		return ErrEntityInvalid
	}
	filename = strings.TrimSpace(filename)
	if filename == "" || len(filename) > maxFilenameLen ||
		strings.ContainsAny(filename, "/\\") || filename == "." || filename == ".." {
		return ErrFilenameInvalid
	}
	for _, r := range filename {
		if r < 32 || r == 0x7f {
			return ErrFilenameInvalid
		}
	}
	if !AllowedMimes[mime] {
		return ErrMimeInvalid
	}
	if size < 1 || size > limits.MaxSizeBytes {
		return ErrTooLarge
	}
	if !checksumPattern.MatchString(checksum) {
		return ErrChecksumInvalid
	}
	return nil
}

// ValidID reports whether id is shaped like the uuids this service issues
// (and is therefore safe as an object-key segment).
func ValidID(id string) bool { return idPattern.MatchString(id) }
