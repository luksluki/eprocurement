package jwt

import (
	"crypto/ed25519"
	"testing"
	"time"
)

type testClaims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

func TestSignParseRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	kid := Kid(pub)

	now := time.Unix(1_700_000_000, 0)
	token, err := Sign(priv, kid, testClaims{Sub: "user-1", Exp: now.Add(time.Minute).Unix()}, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	var got testClaims
	if err := Parse(pub, token, &got, now); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Sub != "user-1" {
		t.Fatalf("sub = %q, want user-1", got.Sub)
	}
}

func TestParseRejectsWrongKey(t *testing.T) {
	_, priv, _ := GenerateKey()
	otherPub, _, _ := GenerateKey()

	token, err := Sign(priv, "kid", testClaims{Sub: "u"}, time.Now())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	var got testClaims
	if err := Parse(otherPub, token, &got, time.Now()); err != ErrSignature {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
}

func TestParseRejectsTamperedPayload(t *testing.T) {
	pub, priv, _ := GenerateKey()
	token, _ := Sign(priv, "kid", testClaims{Sub: "user-1", Exp: time.Now().Add(time.Minute).Unix()}, time.Now())

	// Flip a character inside the payload segment.
	tampered := token[:20] + "X" + token[21:]
	var got testClaims
	if err := Parse(pub, tampered, &got, time.Now()); err == nil {
		t.Fatal("expected error for tampered token")
	}
}

func TestParseRejectsExpired(t *testing.T) {
	pub, priv, _ := GenerateKey()
	now := time.Unix(1_700_000_000, 0)
	token, _ := Sign(priv, "kid", testClaims{Sub: "u", Exp: now.Add(-time.Second).Unix()}, now)

	var got testClaims
	if err := Parse(pub, token, &got, now); err != ErrExpired {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestParseRejectsUnexpectedAlgorithm(t *testing.T) {
	pub, priv, _ := GenerateKey()
	// Hand-build a token whose header claims HS256.
	hdr := []byte(`{"alg":"HS256","typ":"JWT"}`)
	payload := []byte(`{"sub":"u"}`)
	sig := ed25519.Sign(priv, append(append(hdr, '.'), payload...))
	token := b64(hdr) + "." + b64(payload) + "." + b64(sig)

	var got testClaims
	err := Parse(pub, token, &got, time.Now())
	if err == nil || err == ErrMalformed {
		t.Fatalf("err = %v, want algorithm rejection", err)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	pub, _, _ := GenerateKey()
	var got testClaims
	for _, token := range []string{"", "a.b", "a.b.c.d", "!!!.???.***"} {
		if err := Parse(pub, token, &got, time.Now()); err == nil {
			t.Fatalf("expected error for %q", token)
		}
	}
}

func TestKidStableAndShort(t *testing.T) {
	pub, _, _ := GenerateKey()
	if len(Kid(pub)) != 16 {
		t.Fatalf("kid length = %d, want 16 hex chars", len(Kid(pub)))
	}
	pub2, _, _ := GenerateKey()
	if Kid(pub) == Kid(pub2) {
		t.Fatal("distinct keys must produce distinct kids")
	}
}
