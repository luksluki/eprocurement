// Package token verifies gateway access tokens against the identity service's
// JWKS, with a per-kid cache. An unknown kid triggers one synchronous refresh
// before failing, so a key rotation on the identity side is picked up
// immediately instead of waiting for the cache TTL.
package token

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/services/identity/auth"
)

// ErrInvalidToken maps to HTTP 401: bad token, unknown kid even after refresh,
// bad signature, or expired.
var ErrInvalidToken = errors.New("invalid or expired token")

// Key is one public verification key.
type Key struct {
	Kid     string
	Ed25519 ed25519.PublicKey
	Current bool
}

// Fetcher loads the current key set from the identity service.
type Fetcher interface {
	FetchKeys(ctx context.Context) ([]Key, error)
}

// Verifier caches the key set and verifies access tokens.
type Verifier struct {
	fetch Fetcher
	ttl   time.Duration
	now   func() time.Time

	mu      sync.Mutex
	keys    map[string]ed25519.PublicKey
	order   []orderedKey
	fetched time.Time
}

// NewVerifier returns a Verifier refreshing at most once per ttl.
func NewVerifier(fetch Fetcher, ttl time.Duration) *Verifier {
	return &Verifier{
		fetch: fetch,
		ttl:   ttl,
		now:   time.Now,
		keys:  make(map[string]ed25519.PublicKey),
	}
}

// Verify checks the token's signature and expiry and returns its claims.
func (v *Verifier) Verify(ctx context.Context, accessToken string) (auth.AccessClaims, error) {
	var claims auth.AccessClaims

	kid, err := jwt.PeekKid(accessToken)
	if err != nil {
		return claims, ErrInvalidToken
	}
	pub, ok := v.key(ctx, kid)
	if !ok {
		return claims, ErrInvalidToken
	}
	if err := jwt.Parse(pub, accessToken, &claims, v.now()); err != nil {
		return claims, ErrInvalidToken
	}
	return claims, nil
}

// key returns the cached public key for kid, refreshing when the cache is
// stale or the kid is unknown (the rotation window).
func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, bool) {
	v.mu.Lock()
	pub, ok := v.keys[kid]
	stale := v.now().Sub(v.fetched) > v.ttl
	v.mu.Unlock()
	if ok && !stale {
		return pub, true
	}
	if err := v.refresh(ctx); err != nil {
		if ok { // serve the cached key rather than failing on a flaky identity
			return pub, true
		}
		return nil, false
	}
	v.mu.Lock()
	pub, ok = v.keys[kid]
	v.mu.Unlock()
	return pub, ok
}

// KeySet returns the cached keys in JWKS-friendly form, refreshing when stale.
func (v *Verifier) KeySet(ctx context.Context) ([]Key, error) {
	v.mu.Lock()
	stale := v.now().Sub(v.fetched) > v.ttl || len(v.keys) == 0
	v.mu.Unlock()
	if stale {
		if err := v.refresh(ctx); err != nil {
			return nil, err
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]Key, 0, len(v.order))
	for _, k := range v.order {
		if pub, ok := v.keys[k.Kid]; ok {
			out = append(out, Key{Kid: k.Kid, Ed25519: pub, Current: k.Current})
		}
	}
	return out, nil
}

// order preserves the serving order (current first) for the JWKS response.
type orderedKey struct {
	Kid     string
	Current bool
}

// order is guarded by mu alongside keys.
func (v *Verifier) refresh(ctx context.Context) error {
	keys, err := v.fetch.FetchKeys(ctx)
	if err != nil {
		return err
	}
	byKid := make(map[string]ed25519.PublicKey, len(keys))
	order := make([]orderedKey, 0, len(keys))
	for _, k := range keys {
		byKid[k.Kid] = k.Ed25519
		order = append(order, orderedKey{Kid: k.Kid, Current: k.Current})
	}

	v.mu.Lock()
	v.keys = byKid
	v.order = order
	v.fetched = v.now()
	v.mu.Unlock()
	return nil
}
