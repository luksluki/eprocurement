package token

import (
	"context"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

type rotatingFetcher struct {
	keys    []Key
	fetches int
	err     error
}

func (f *rotatingFetcher) FetchKeys(ctx context.Context) ([]Key, error) {
	f.fetches++
	if f.err != nil {
		return nil, f.err
	}
	return f.keys, nil
}

func TestVerifierCacheAndRotation(t *testing.T) {
	oldPub, oldPriv, _ := jwt.GenerateKey()
	newPub, newPriv, _ := jwt.GenerateKey()
	oldKid, newKid := jwt.Kid(oldPub), jwt.Kid(newPub)

	fetcher := &rotatingFetcher{keys: []Key{{Kid: oldKid, Ed25519: oldPub, Current: true}}}
	verifier := NewVerifier(fetcher, time.Hour) // long TTL: refreshes only for unknown kids

	now := time.Now()
	oldToken, err := jwt.Sign(oldPriv, oldKid, auth.AccessClaims{Sub: "u1", EXP: now.Add(time.Minute).Unix()}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Happy path: token verifies; the fetch is cached.
	claims, err := verifier.Verify(t.Context(), oldToken)
	if err != nil || claims.Sub != "u1" {
		t.Fatalf("verify: claims=%+v err=%v", claims, err)
	}
	if fetcher.fetches != 1 {
		t.Fatalf("fetches = %d, want 1 (cached afterwards)", fetcher.fetches)
	}
	if _, err := verifier.Verify(t.Context(), oldToken); err != nil {
		t.Fatalf("second verify should hit the cache: %v", err)
	}
	if fetcher.fetches != 1 {
		t.Fatalf("fetches = %d after cache hit, want 1", fetcher.fetches)
	}

	// Rotation: identity now serves new (current) + old (previous).
	fetcher.keys = []Key{
		{Kid: newKid, Ed25519: newPub, Current: true},
		{Kid: oldKid, Ed25519: oldPub, Current: false},
	}
	newToken, _ := jwt.Sign(newPriv, newKid, auth.AccessClaims{Sub: "u2", EXP: now.Add(time.Minute).Unix()}, now)

	// The unknown-kid token forces exactly one refresh, then verifies.
	if _, err := verifier.Verify(t.Context(), newToken); err != nil {
		t.Fatalf("verify new-kid token after rotation: %v", err)
	}
	if fetcher.fetches != 2 {
		t.Fatalf("fetches = %d, want 2 (unknown kid triggered refresh)", fetcher.fetches)
	}

	// AC: old tokens stay valid until expiry — the old kid is still in the set.
	if _, err := verifier.Verify(t.Context(), oldToken); err != nil {
		t.Fatalf("old-kid token must still verify during rotation: %v", err)
	}
}

func TestVerifierRejects(t *testing.T) {
	pub, priv, _ := jwt.GenerateKey()
	kid := jwt.Kid(pub)
	fetcher := &rotatingFetcher{keys: []Key{{Kid: kid, Ed25519: pub, Current: true}}}
	verifier := NewVerifier(fetcher, time.Hour)

	now := time.Now()
	valid, _ := jwt.Sign(priv, kid, auth.AccessClaims{Sub: "u", EXP: now.Add(time.Minute).Unix()}, now)

	if _, err := verifier.Verify(t.Context(), "not-a-token"); err != ErrInvalidToken {
		t.Fatalf("garbage token err = %v, want ErrInvalidToken", err)
	}

	// Token signed by a key the identity never served → 401 after refresh.
	_, strangerPriv, _ := jwt.GenerateKey()
	strangerToken, _ := jwt.Sign(strangerPriv, "stranger", auth.AccessClaims{Sub: "u", EXP: now.Add(time.Minute).Unix()}, now)
	if _, err := verifier.Verify(t.Context(), strangerToken); err != ErrInvalidToken {
		t.Fatalf("unknown kid err = %v, want ErrInvalidToken", err)
	}

	// Expired.
	expired, _ := jwt.Sign(priv, kid, auth.AccessClaims{Sub: "u", EXP: now.Add(-time.Minute).Unix()}, now)
	if _, err := verifier.Verify(t.Context(), expired); err != ErrInvalidToken {
		t.Fatalf("expired err = %v, want ErrInvalidToken", err)
	}

	// Tampered signature against a known kid.
	tampered := valid[:len(valid)-3] + "AAA"
	if _, err := verifier.Verify(t.Context(), tampered); err != ErrInvalidToken {
		t.Fatalf("tampered err = %v, want ErrInvalidToken", err)
	}
}

func TestVerifierKeySetServesOrderAndShape(t *testing.T) {
	pub, _, _ := jwt.GenerateKey()
	prevPub, _, _ := jwt.GenerateKey()
	kid, prevKid := jwt.Kid(pub), jwt.Kid(prevPub)
	fetcher := &rotatingFetcher{keys: []Key{
		{Kid: kid, Ed25519: pub, Current: true},
		{Kid: prevKid, Ed25519: prevPub, Current: false},
	}}
	verifier := NewVerifier(fetcher, time.Hour)

	keys, err := verifier.KeySet(t.Context())
	if err != nil {
		t.Fatalf("KeySet: %v", err)
	}
	if len(keys) != 2 || keys[0].Kid != kid || !keys[0].Current || keys[1].Current {
		t.Fatalf("keys = %+v, want current first then previous", keys)
	}
}
