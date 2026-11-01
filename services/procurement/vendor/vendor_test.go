package vendor

import (
	"errors"
	"strings"
	"testing"
)

// Domain-only unit tests (#57): Normalize, ValidateCreate, ValidateUpdate,
// ValidateList, and escapeLike with no database — the DB-backed store suite
// (store_test.go, store_list_test.go) covers the same rules end-to-end and
// skips without TEST_DATABASE_URL.

func TestNormalize(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"lowercased input":   {"dup-01", "DUP-01"},
		"surrounding space":  {"  DUP-01\t", "DUP-01"},
		"already normalized": {"DUP-01", "DUP-01"},
		"inner case kept":    {"Dup-01", "DUP-01"},
		"empty stays empty":  {"", ""},
	} {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("%s: Normalize(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// The canonical-form matrix for ValidateCreate: code pattern (2..32 chars of
// [A-Z0-9-], no underscore/space/lowercase), name 2..200 after trim, and the
// optional email shape. Boundary lengths included.
func TestValidateCreate(t *testing.T) {
	longName := strings.Repeat("N", 200)
	tooLongName := strings.Repeat("N", 201)
	code32 := strings.Repeat("A", 32)
	code33 := strings.Repeat("A", 33)

	for name, tc := range map[string]struct {
		code, name, email string
		want              error
	}{
		"minimal valid":       {"DUP-01", "PT Valid", "", nil},
		"with valid email":    {"DUP-01", "PT Valid", "ops@example.com", nil},
		"code at max length":  {code32, "PT Valid", "", nil},
		"code one too long":   {code33, "PT Valid", "", ErrCodeInvalid},
		"code one char":       {"D", "PT Valid", "", ErrCodeInvalid},
		"code empty":          {"", "PT Valid", "", ErrCodeInvalid},
		"code lowercase":      {"dup-01", "PT Valid", "", ErrCodeInvalid},
		"code underscore":     {"DUP_01", "PT Valid", "", ErrCodeInvalid},
		"code with space":     {"DUP 01", "PT Valid", "", ErrCodeInvalid},
		"code leading hyphen": {"-DUP01", "PT Valid", "", ErrCodeInvalid},
		"name at min length":  {"DUP-01", "PT", "", nil},
		"name one char":       {"DUP-01", "X", "", ErrNameInvalid},
		"name empty":          {"DUP-01", "", "", ErrNameInvalid},
		"name blank trims":    {"DUP-01", "   ", "", ErrNameInvalid},
		"name at max length":  {"DUP-01", longName, "", nil},
		"name one too long":   {"DUP-01", tooLongName, "", ErrNameInvalid},
		"email empty is none": {"DUP-01", "PT Valid", "", nil},
		"email no at-sign":    {"DUP-01", "PT Valid", "not-an-email", ErrEmailInvalid},
		"email no domain":     {"DUP-01", "PT Valid", "ops@", ErrEmailInvalid},
		"email no tld dot":    {"DUP-01", "PT Valid", "ops@example", ErrEmailInvalid},
		"email with spaces":   {"DUP-01", "PT Valid", "ops @example.com", ErrEmailInvalid},
		"first error wins":    {"bad", "X", "nope@", ErrCodeInvalid},
	} {
		if err := ValidateCreate(tc.code, tc.name, tc.email); !errors.Is(err, tc.want) {
			t.Errorf("%s: ValidateCreate(%q, %q, %q) = %v, want %v", name, tc.code, tc.name, tc.email, err, tc.want)
		}
	}
}

// ValidateUpdate checks only the mutable fields; empty means "keep the
// stored value", so both-empty is legal.
func TestValidateUpdate(t *testing.T) {
	for name, tc := range map[string]struct {
		name, email string
		want        error
	}{
		"both empty keeps stored": {"", "", nil},
		"valid rename":            {"PT Renamed", "", nil},
		"valid email change":      {"", "new@example.com", nil},
		"both valid":              {"PT Renamed", "new@example.com", nil},
		"name one char":           {"X", "", ErrNameInvalid},
		"name blank":              {"   ", "", ErrNameInvalid},
		"name too long":           {strings.Repeat("N", 201), "", ErrNameInvalid},
		"email invalid":           {"PT Renamed", "nope@", ErrEmailInvalid},
		"name error wins":         {"X", "nope@", ErrNameInvalid},
	} {
		if err := ValidateUpdate(tc.name, tc.email); !errors.Is(err, tc.want) {
			t.Errorf("%s: ValidateUpdate(%q, %q) = %v, want %v", name, tc.name, tc.email, err, tc.want)
		}
	}
}

// ValidateList normalization: status allowlist (after trim), page clamped
// into [1, MaxPage], page_size defaulted and capped, and the query
// uppercased, trimmed, and LIKE-escaped.
func TestValidateList(t *testing.T) {
	for name, tc := range map[string]struct {
		in      ListFilter
		want    ListFilter
		wantErr error
	}{
		"zero values get defaults": {
			ListFilter{},
			ListFilter{Page: 1, PageSize: DefaultPageSize},
			nil,
		},
		"status active passes": {
			ListFilter{Status: StatusActive, Page: 2, PageSize: 10},
			ListFilter{Status: StatusActive, Page: 2, PageSize: 10},
			nil,
		},
		"status trimmed": {
			ListFilter{Status: "  active  "},
			ListFilter{Status: StatusActive, Page: 1, PageSize: DefaultPageSize},
			nil,
		},
		"status deactivated passes": {
			ListFilter{Status: StatusDeactivated},
			ListFilter{Status: StatusDeactivated, Page: 1, PageSize: DefaultPageSize},
			nil,
		},
		"unknown status rejected": {
			ListFilter{Status: "paused"},
			ListFilter{},
			ErrStatusInvalid,
		},
		"negative page clamped to 1": {
			ListFilter{Page: -5},
			ListFilter{Page: 1, PageSize: DefaultPageSize},
			nil,
		},
		"page at max passes": {
			ListFilter{Page: MaxPage},
			ListFilter{Page: MaxPage, PageSize: DefaultPageSize},
			nil,
		},
		"page beyond max rejected": {
			ListFilter{Page: MaxPage + 1},
			ListFilter{},
			ErrPageInvalid,
		},
		"page size capped": {
			ListFilter{PageSize: 500},
			ListFilter{Page: 1, PageSize: MaxPageSize},
			nil,
		},
		"query uppercased and trimmed": {
			ListFilter{Query: "  dup-01  "},
			ListFilter{Query: "DUP-01", Page: 1, PageSize: DefaultPageSize},
			nil,
		},
		"query wildcards escaped": {
			ListFilter{Query: `100%\_x`},
			// % and _ gain a backslash; the input backslash passes
			// through untouched (see TestEscapeLike) and X is uppercased.
			ListFilter{Query: `100\%\\_X`, Page: 1, PageSize: DefaultPageSize},
			nil,
		},
	} {
		got, err := ValidateList(tc.in)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
			continue
		}
		// On error the returned filter is incidental (the input with the
		// status already trimmed); only the error is asserted.
		if tc.wantErr == nil && got != tc.want {
			t.Errorf("%s: filter = %+v, want %+v", name, got, tc.want)
		}
	}
}

// escapeLike neutralizes the % and _ LIKE metacharacters. A literal
// backslash currently passes through unescaped (the replacer maps `\` to
// itself), so a query containing `\` is not matched literally today — the
// test pins that behavior; flipping the replacer pair to `\`→`\\` is the
// recorded follow-up and this table is where its test changes.
func TestEscapeLike(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"percent":             {"100%", `100\%`},
		"underscore":          {"a_b", `a\_b`},
		"backslash unescaped": {`a\b`, `a\b`},
		"combined metachars":  {`%\_`, `\%\\_`},
		"plain text intact":   {"DUP-01", "DUP-01"},
		"empty string":        {"", ""},
	} {
		if got := escapeLike(tc.in); got != tc.want {
			t.Errorf("%s: escapeLike(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}
