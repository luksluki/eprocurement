package attachment

import (
	"strings"
	"testing"
)

// 64 lowercase hex chars ("ab" x32) — a valid sha256 declaration.
const validChecksum = "abababababababababababababababababababababababababababababababab"

func TestValidateCreate(t *testing.T) {
	limits := Limits{MaxSizeBytes: 25 << 20}
	base := func() (string, string, string, string, int64, string) {
		return EntityTypeVendor, "dddddddd-0000-0000-0000-000000000001",
			"contract-2026.pdf", "application/pdf", 1024, validChecksum
	}

	tests := []struct {
		name    string
		mutate  func(entityType, entityID, filename, mime *string, size *int64, checksum *string)
		wantErr error
	}{
		{"valid", func(a, b, c, d *string, e *int64, f *string) {}, nil},
		{"entity not vendor", func(a, b, c, d *string, e *int64, f *string) { *a = "requisition" }, ErrEntityInvalid},
		{"entity id with slash", func(a, b, c, d *string, e *int64, f *string) { *b = "../escape" }, ErrEntityInvalid},
		{"entity id empty", func(a, b, c, d *string, e *int64, f *string) { *b = "" }, ErrEntityInvalid},
		{"filename empty", func(a, b, c, d *string, e *int64, f *string) { *c = "" }, ErrFilenameInvalid},
		{"filename path separator", func(a, b, c, d *string, e *int64, f *string) { *c = "a/b.pdf" }, ErrFilenameInvalid},
		{"filename traversal", func(a, b, c, d *string, e *int64, f *string) { *c = ".." }, ErrFilenameInvalid},
		{"filename too long", func(a, b, c, d *string, e *int64, f *string) { *c = strings.Repeat("x", 256) }, ErrFilenameInvalid},
		{"filename control char", func(a, b, c, d *string, e *int64, f *string) { *c = "bad\x00name" }, ErrFilenameInvalid},
		{"mime not allowed", func(a, b, c, d *string, e *int64, f *string) { *d = "application/x-msdownload" }, ErrMimeInvalid},
		{"mime uppercase allowed after normalize", func(a, b, c, d *string, e *int64, f *string) { *d = "Application/PDF" }, nil},
		{"size zero", func(a, b, c, d *string, e *int64, f *string) { *e = 0 }, ErrTooLarge},
		{"size above limit", func(a, b, c, d *string, e *int64, f *string) { *e = limits.MaxSizeBytes + 1 }, ErrTooLarge},
		{"checksum short", func(a, b, c, d *string, e *int64, f *string) { *f = "abcd" }, ErrChecksumInvalid},
		{"checksum not hex", func(a, b, c, d *string, e *int64, f *string) { *f = strings.Repeat("zz", 32) }, ErrChecksumInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entityType, entityID, filename, mime, size, checksum := base()
			tt.mutate(&entityType, &entityID, &filename, &mime, &size, &checksum)
			mime, checksum = NormalizeCanonical(mime, checksum)
			err := ValidateCreate(limits, entityType, entityID, filename, mime, size, checksum)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateCreate: %v", err)
				}
				return
			}
			if err != tt.wantErr {
				t.Fatalf("ValidateCreate error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestObjectKeyLayout(t *testing.T) {
	key := ObjectKey("t-1", EntityTypeVendor, "a-2")
	if key != "attachments/t-1/vendor/a-2" {
		t.Fatalf("ObjectKey = %q", key)
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"a", "dddddddd-0000-0000-0000-000000000001", "A-9"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "/etc", "a b", "..", "a/b"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}
