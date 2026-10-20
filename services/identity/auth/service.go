// Package auth implements the identity service's credential verification and
// token lifecycle: login with argon2id, EdDSA access JWTs, and refresh token
// rotation with family revocation.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/ratelimit"
)

// Token lifetimes per PRD §17: access 15 minutes, refresh 7 days.
const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 7 * 24 * time.Hour
)

// ErrInvalidCredentials is returned for every login failure (unknown email,
// wrong password, ambiguous email, disabled user) with an identical message,
// so callers cannot enumerate users.
var ErrInvalidCredentials = errors.New("invalid email or password")

// ErrRateLimited is returned when the login rate limit for the key is
// exceeded.
var ErrRateLimited = errors.New("too many login attempts, try again later")

// ErrInvalidRefreshToken is returned for every refresh failure (unknown,
// expired, revoked, or reused token) and for logout lookups.
var ErrInvalidRefreshToken = errors.New("invalid refresh token")

// TokenPair is the result of a successful login or refresh.
type TokenPair struct {
	AccessToken      string
	AccessExpiresIn  int64
	RefreshToken     string
	RefreshExpiresIn int64
}

type userRecord struct {
	ID           string
	TenantID     string
	Status       string
	PasswordHash string
}

// SigningKey is one Ed25519 signing key with its derived kid.
type SigningKey struct {
	Priv ed25519.PrivateKey
	Kid  string
}

// Keyring holds the active signing keys: Current signs new tokens; Previous
// (optional) stays verifiable during rotation until the last token it signed
// has expired (15 minutes at most).
type Keyring struct {
	Current  SigningKey
	Previous *SigningKey // nil when no rotation is in progress
}

// NewKeyring builds a Keyring from private keys, deriving each kid. A
// previous key identical to the current one is dropped — it would be a no-op
// rotation serving a single key anyway.
func NewKeyring(current ed25519.PrivateKey, previous ed25519.PrivateKey) Keyring {
	kr := Keyring{
		Current: SigningKey{Priv: current, Kid: jwt.Kid(current.Public().(ed25519.PublicKey))},
	}
	if previous != nil {
		prev := SigningKey{Priv: previous, Kid: jwt.Kid(previous.Public().(ed25519.PublicKey))}
		if prev.Kid != kr.Current.Kid {
			kr.Previous = &prev
		}
	}
	return kr
}

// Jwk is one public key in the key set served to verifiers.
type Jwk struct {
	Kid     string
	Ed25519 ed25519.PublicKey
	Current bool
}

// Service implements the auth flows on top of PostgreSQL.
type Service struct {
	db           *pgxpool.Pool
	log          *slog.Logger
	keys         Keyring
	loginLimiter *ratelimit.Limiter
	now          func() time.Time
}

// New builds a Service. keys carries the Ed25519 signing keys; a previous key
// keeps tokens issued before a rotation verifiable (see docs/ops/jwks-rotation.md).
func New(db *pgxpool.Pool, log *slog.Logger, keys Keyring, loginRatePerMin int) *Service {
	return &Service{
		db:           db,
		log:          log,
		keys:         keys,
		loginLimiter: ratelimit.New(float64(loginRatePerMin)/60.0, loginRatePerMin),
		now:          time.Now,
	}
}

// Jwks returns the active public keys, current first.
func (s *Service) Jwks() []Jwk {
	keys := []Jwk{{
		Kid:     s.keys.Current.Kid,
		Ed25519: s.keys.Current.Priv.Public().(ed25519.PublicKey),
		Current: true,
	}}
	if s.keys.Previous != nil {
		keys = append(keys, Jwk{
			Kid:     s.keys.Previous.Kid,
			Ed25519: s.keys.Previous.Priv.Public().(ed25519.PublicKey),
			Current: false,
		})
	}
	return keys
}

// PublicKey returns the current Ed25519 public key that verifies access tokens.
func (s *Service) PublicKey() ed25519.PublicKey {
	return s.keys.Current.Priv.Public().(ed25519.PublicKey)
}

// AccessClaims is the JWT payload carried by every access token.
type AccessClaims struct {
	Iss      string   `json:"iss"`
	Sub      string   `json:"sub"`
	TenantID string   `json:"tid"`
	Roles    []string `json:"roles,omitempty"`
	JTI      string   `json:"jti"`
	IAT      int64    `json:"iat"`
	EXP      int64    `json:"exp"`
}

const issuer = "identity.eprocurement"

// Login verifies credentials and returns a fresh token pair. rateKey is
// typically "email|peer-address".
func (s *Service) Login(ctx context.Context, email, password, rateKey string) (TokenPair, error) {
	if !s.loginLimiter.Allow(rateKey) {
		return TokenPair{}, ErrRateLimited
	}

	u, roles, err := s.loadUser(ctx, email)
	if err != nil {
		return TokenPair{}, err
	}
	if u == nil {
		DummyVerify(password) // equalize timing with the known-email path
		return TokenPair{}, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		s.log.ErrorContext(ctx, "malformed stored password hash", "user_id", u.ID, "error", err.Error())
		return TokenPair{}, ErrInvalidCredentials
	}
	if !ok || u.Status != "active" {
		return TokenPair{}, ErrInvalidCredentials
	}
	return s.issuePair(ctx, u, roles)
}

// Refresh exchanges a valid refresh token for a new pair and rotates the old
// token. Presenting a rotated token revokes the entire family (theft signal).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	sum := sha256.Sum256([]byte(refreshToken))

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TokenPair{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id        string
		userID    string
		expiresAt time.Time
		rotatedTo *string
		revokedAt *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT id, user_id, expires_at, rotated_to, revoked_at
		FROM identity.refresh_tokens
		WHERE token_hash = $1
		FOR UPDATE`, sum[:]).Scan(&id, &userID, &expiresAt, &rotatedTo, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, ErrInvalidRefreshToken
	}
	if err != nil {
		return TokenPair{}, err
	}

	now := s.now()
	if revokedAt != nil || expiresAt.Before(now) {
		return TokenPair{}, ErrInvalidRefreshToken
	}
	if rotatedTo != nil {
		// Reuse of an already-rotated token: revoke the whole family.
		if _, err := tx.Exec(ctx, `
			WITH RECURSIVE family AS (
				SELECT id, rotated_to FROM identity.refresh_tokens WHERE id = $1
				UNION
				SELECT r.id, r.rotated_to FROM identity.refresh_tokens r
				JOIN family f ON r.rotated_to = f.id OR r.id = f.rotated_to
			)
			UPDATE identity.refresh_tokens SET revoked_at = $2
			WHERE id IN (SELECT id FROM family) AND revoked_at IS NULL`, id, now); err != nil {
			return TokenPair{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenPair{}, err
		}
		s.log.WarnContext(ctx, "refresh token reuse detected; token family revoked", "token_id", id)
		return TokenPair{}, ErrInvalidRefreshToken
	}

	u, roles, err := s.loadUserByID(ctx, tx, userID)
	if err != nil {
		return TokenPair{}, err
	}
	if u == nil || u.Status != "active" {
		return TokenPair{}, ErrInvalidRefreshToken
	}

	pair, err := s.issuePairTx(ctx, tx, u, roles)
	if err != nil {
		return TokenPair{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.refresh_tokens SET rotated_to = $1 WHERE id = $2`, pair.id, id); err != nil {
		return TokenPair{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair.pair, nil
}

// Logout revokes the given refresh token. It is idempotent and returns the
// same result whether or not the token existed.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	sum := sha256.Sum256([]byte(refreshToken))
	_, err := s.db.Exec(ctx, `
		UPDATE identity.refresh_tokens SET revoked_at = $1
		WHERE token_hash = $2 AND revoked_at IS NULL`, s.now(), sum[:])
	return err
}

type issuedRefresh struct {
	id   string
	pair TokenPair
}

// dbtx is the subset of *pgxpool.Pool and pgx.Tx the issue path needs.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadUser returns the single active-schema user for the email plus its
// roles, or nil when the email is unknown or ambiguous (the schema allows the
// same address in several tenants).
func (s *Service) loadUser(ctx context.Context, email string) (*userRecord, []string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT u.id, u.tenant_id, u.status, u.password_hash,
		       COALESCE(array_agg(r.code ORDER BY r.code) FILTER (WHERE r.code IS NOT NULL), '{}')
		FROM identity.users u
		LEFT JOIN identity.user_roles ur ON ur.user_id = u.id
		LEFT JOIN identity.roles r ON r.id = ur.role_id
		WHERE lower(u.email) = lower($1)
		GROUP BY u.id`, email)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var users []*userRecord
	var roleSets [][]string
	for rows.Next() {
		var u userRecord
		var roles []string
		if err := rows.Scan(&u.ID, &u.TenantID, &u.Status, &u.PasswordHash, &roles); err != nil {
			return nil, nil, err
		}
		users = append(users, &u)
		roleSets = append(roleSets, roles)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(users) != 1 {
		// Zero matches or the same address in several tenants: no user.
		return nil, nil, nil
	}
	return users[0], roleSets[0], nil
}

func (s *Service) loadUserByID(ctx context.Context, tx pgx.Tx, userID string) (*userRecord, []string, error) {
	var (
		u     userRecord
		roles []string
	)
	err := tx.QueryRow(ctx, `
		SELECT u.id, u.tenant_id, u.status,
		       COALESCE(array_agg(r.code ORDER BY r.code) FILTER (WHERE r.code IS NOT NULL), '{}')
		FROM identity.users u
		LEFT JOIN identity.user_roles ur ON ur.user_id = u.id
		LEFT JOIN identity.roles r ON r.id = ur.role_id
		WHERE u.id = $1
		GROUP BY u.id`, userID).Scan(&u.ID, &u.TenantID, &u.Status, &roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &u, roles, nil
}

func (s *Service) issuePair(ctx context.Context, u *userRecord, roles []string) (TokenPair, error) {
	issued, err := s.issuePairTx(ctx, s.db, u, roles)
	return issued.pair, err
}

func (s *Service) issuePairTx(ctx context.Context, tx dbtx, u *userRecord, roles []string) (issuedRefresh, error) {
	now := s.now()

	jti := make([]byte, 8)
	if _, err := rand.Read(jti); err != nil {
		return issuedRefresh{}, err
	}
	claims := AccessClaims{
		Iss:      issuer,
		Sub:      u.ID,
		TenantID: u.TenantID,
		Roles:    roles,
		JTI:      hex.EncodeToString(jti),
		IAT:      now.Unix(),
		EXP:      now.Add(AccessTTL).Unix(),
	}
	access, err := jwt.Sign(s.keys.Current.Priv, s.keys.Current.Kid, claims, now)
	if err != nil {
		return issuedRefresh{}, err
	}

	refreshRaw := make([]byte, 32)
	if _, err := rand.Read(refreshRaw); err != nil {
		return issuedRefresh{}, err
	}
	refresh := hex.EncodeToString(refreshRaw)
	// Store the hash of the exact client-visible string so lookups in
	// Refresh/Logout hash the same value.
	sum := sha256.Sum256([]byte(refresh))

	var refreshID string
	err = tx.QueryRow(ctx, `
		INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id`, u.ID, sum[:], now.Add(RefreshTTL)).Scan(&refreshID)
	if err != nil {
		return issuedRefresh{}, err
	}

	return issuedRefresh{
		id: refreshID,
		pair: TokenPair{
			AccessToken:      access,
			AccessExpiresIn:  int64(AccessTTL.Seconds()),
			RefreshToken:     refresh,
			RefreshExpiresIn: int64(RefreshTTL.Seconds()),
		},
	}, nil
}
