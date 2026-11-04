package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (RFC 9106 second recommended option, scaled for
// interactive login on a laptop): 64 MiB, 2 passes, 4 lanes.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonSaltLen = 16
	argonKeyLen  = 32
)

// HashPassword returns the PHC string format for argon2id:
// $argon2id$v=19$m=65536,t=2,p=4$<salt-b64>$<hash-b64>
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return phcEncode(salt, key), nil
}

// VerifyPassword reports whether password matches the stored PHC string.
// It returns an error only for malformed stored hashes; a wrong password is
// a plain false.
func VerifyPassword(password, stored string) (bool, error) {
	params, salt, want, err := phcDecode(stored)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	time    uint32
	memory  uint32
	threads uint8
}

// dummyHash makes the unknown-email path cost the same as the known-email
// path so login timing does not reveal whether an account exists.
var dummyHash = func() string {
	h, err := HashPassword("timing-equalization-dummy-password")
	if err != nil {
		panic("auth: hash dummy: " + err.Error())
	}
	return h
}()

// DummyVerify burns one argon2id verification against a fixed dummy hash.
func DummyVerify(password string) {
	_, _ = VerifyPassword(password, dummyHash)
}

func phcEncode(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func phcDecode(stored string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(stored, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, fmt.Errorf("auth: malformed hash: not an argon2id PHC string")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, fmt.Errorf("auth: unsupported argon2 version in %q", parts[2])
	}

	p := argonParams{}
	for _, kv := range strings.Split(parts[3], ",") {
		var v uint32
		switch {
		case strings.HasPrefix(kv, "m="):
			if _, err := fmt.Sscanf(kv, "m=%d", &v); err != nil {
				return argonParams{}, nil, nil, fmt.Errorf("auth: malformed memory param %q", kv)
			}
			p.memory = v
		case strings.HasPrefix(kv, "t="):
			if _, err := fmt.Sscanf(kv, "t=%d", &v); err != nil {
				return argonParams{}, nil, nil, fmt.Errorf("auth: malformed time param %q", kv)
			}
			p.time = v
		case strings.HasPrefix(kv, "p="):
			var t uint32
			if _, err := fmt.Sscanf(kv, "p=%d", &t); err != nil {
				return argonParams{}, nil, nil, fmt.Errorf("auth: malformed threads param %q", kv)
			}
			if t < 1 || t > 255 {
				return argonParams{}, nil, nil, fmt.Errorf("auth: threads out of range %q", kv)
			}
			p.threads = uint8(t)
		default:
			return argonParams{}, nil, nil, fmt.Errorf("auth: unknown param %q", kv)
		}
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return argonParams{}, nil, nil, fmt.Errorf("auth: incomplete argon2 params in %q", parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("auth: decode salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("auth: decode hash: %w", err)
	}
	if len(salt) == 0 || len(key) == 0 {
		return argonParams{}, nil, nil, fmt.Errorf("auth: empty salt or hash")
	}
	return p, salt, key, nil
}
