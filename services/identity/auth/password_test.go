package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=2,p=4$") {
		t.Fatalf("hash is not the expected PHC format: %s", hash)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}

	ok, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword error: %v", err)
	}
	if ok {
		t.Fatal("wrong password accepted")
	}
}

func TestHashIsSalted(t *testing.T) {
	h1, _ := HashPassword("same password")
	h2, _ := HashPassword("same password")
	if h1 == h2 {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, stored := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=2,p=4$c2FsdA$aGFzaA", // wrong variant
		"$argon2id$v=19$m=0,t=2,p=4$c2FsdA$aGFzaA",    // zero memory
		"$argon2id$v=19$m=65536,t=2$c2FsdA$aGFzaA",    // missing param
		"$argon2id$v=19$m=65536,t=2,p=4$$aGFzaA",      // empty salt
	} {
		if _, err := VerifyPassword("x", stored); err == nil {
			t.Errorf("expected error for malformed hash %q", stored)
		}
	}
}

func TestDummyVerifyIsSafe(t *testing.T) {
	DummyVerify("anything") // must not panic; cost only
}

// A fixed PHC vector pins the hashing parameters: any accidental change to
// time/memory/threads or the encoding breaks existing stored hashes, and this
// test fails before a single user is written with the new parameters.
func TestPasswordHashVector(t *testing.T) {
	const vector = "$argon2id$v=19$m=65536,t=2,p=4$wuAo39rHwgFKqUYm6dEypw$B2Ff0RNOx+eLm5c/8wg7Zxymh3VGfVMIP3IBeN3vTZs"

	ok, err := VerifyPassword("vector-password-2026", vector)
	if err != nil {
		t.Fatalf("vector verify error: %v", err)
	}
	if !ok {
		t.Fatal("fixed vector no longer verifies — hashing parameters changed?")
	}

	ok, _ = VerifyPassword("wrong", vector)
	if ok {
		t.Fatal("wrong password accepted against vector")
	}
}
