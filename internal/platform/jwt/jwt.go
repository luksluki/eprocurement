// Package jwt implements the small slice of JWT we need: EdDSA-signed
// access tokens with a kid header, signed and verified with crypto/ed25519
// from the standard library. No other algorithm is accepted, which rules out
// algorithm-confusion attacks by construction.
//
// Signature verification and claim decoding are separated from claim
// semantics: Parse checks the signature and expiry, the caller decides what
// the claims mean.
package jwt

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const algEdDSA = "EdDSA"

var (
	ErrMalformed = errors.New("jwt: malformed token")
	ErrSignature = errors.New("jwt: signature verification failed")
	ErrExpired   = errors.New("jwt: token expired")
	ErrAlgorithm = errors.New("jwt: unexpected algorithm")
)

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid,omitempty"`
}

// Kid derives a stable key id from the public key (first 8 bytes of its
// sha256, hex-encoded) so tokens carry the key that signed them.
func Kid(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// GenerateKey returns a new Ed25519 key pair for signing access tokens.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// Sign returns a compact JWT for the claims, signed with priv.
func Sign(priv ed25519.PrivateKey, kid string, claims any, now time.Time) (string, error) {
	hdr, err := json.Marshal(header{Alg: algEdDSA, Typ: "JWT", Kid: kid})
	if err != nil {
		return "", fmt.Errorf("jwt: marshal header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("jwt: marshal claims: %w", err)
	}

	signingInput := b64(hdr) + "." + b64(payload)
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + b64(sig), nil
}

// Parse verifies the token's signature against pub and decodes the claims
// into out. It rejects non-EdDSA tokens and expired exp claims.
func Parse(pub ed25519.PublicKey, token string, out any, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ErrMalformed
	}
	sig, err := unb64(parts[2])
	if err != nil {
		return ErrMalformed
	}
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(pub, []byte(signingInput), sig) {
		return ErrSignature
	}

	hdrBytes, err := unb64(parts[0])
	if err != nil {
		return ErrMalformed
	}
	var hdr header
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return ErrMalformed
	}
	if hdr.Alg != algEdDSA {
		return fmt.Errorf("%w: %s", ErrAlgorithm, hdr.Alg)
	}

	payload, err := unb64(parts[1])
	if err != nil {
		return ErrMalformed
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return ErrMalformed
	}

	var probe struct {
		Exp int64 `json:"exp"`
	}
	_ = json.Unmarshal(payload, &probe)
	if probe.Exp != 0 && now.Unix() >= probe.Exp {
		return ErrExpired
	}
	return nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
