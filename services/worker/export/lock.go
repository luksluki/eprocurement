package export

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis key families (PRD §13: lock:export:nightly, heartbeat
// job:export:last_success — per tenant, maintainer decision 2026-10-27).
const (
	lockPrefix      = "lock:export:nightly:"
	heartbeatPrefix = "job:export:last_success:"
	heartbeatLayout = time.RFC3339Nano
	lockTokenBytes  = 16
)

// LockKey is the per-tenant distributed lock key.
func LockKey(tenantID string) string { return lockPrefix + tenantID }

// HeartbeatKey is the per-tenant last-success timestamp key.
func HeartbeatKey(tenantID string) string { return heartbeatPrefix + tenantID }

// KV is the Redis surface the nightly export needs: SET NX PX acquisition
// with a random token plus compare-and-delete release (the lock), and plain
// get/set (the heartbeat). An interface so the scheduler and tracker tests
// run without a broker.
type KV interface {
	// Acquire sets key=token with NX and the given TTL. acquired is false
	// when the key already exists (someone else holds the lock).
	Acquire(ctx context.Context, key string, ttl time.Duration) (token string, acquired bool, err error)
	// Release deletes key only if it still holds token (Lua
	// compare-and-delete); releasing someone else's lock is a no-op.
	Release(ctx context.Context, key, token string) error
	// Get returns the key's string value; ok is false when absent.
	Get(ctx context.Context, key string) (value string, ok bool, err error)
	// Set stores value without expiry.
	Set(ctx context.Context, key, value string) error
}

// RedisKV implements KV against go-redis v9 (the single approved new
// dependency of #49).
type RedisKV struct {
	rdb redis.UniversalClient
}

// NewRedisKV wraps a connected go-redis client.
func NewRedisKV(rdb redis.UniversalClient) *RedisKV { return &RedisKV{rdb: rdb} }

// Acquire implements SET key token NX PX ttl: exactly one concurrent
// acquirer wins; the TTL bounds a crashed holder.
func (k *RedisKV) Acquire(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	token, err := randomToken()
	if err != nil {
		return "", false, err
	}
	ok, err := k.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("export: acquire %s: %w", key, err)
	}
	return token, ok, nil
}

// releaseScript deletes the lock only when the stored token matches — the
// holder never releases a lock it lost to TTL expiry and re-acquisition.
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`)

// Release runs the compare-and-delete script atomically.
func (k *RedisKV) Release(ctx context.Context, key, token string) error {
	if _, err := releaseScript.Run(ctx, k.rdb, []string{key}, token).Result(); err != nil {
		return fmt.Errorf("export: release %s: %w", key, err)
	}
	return nil
}

// Get returns the key's value; redis.Nil (absent) maps to ok=false, not an
// error.
func (k *RedisKV) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := k.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("export: get %s: %w", key, err)
	}
	return value, true, nil
}

// Set stores value without expiry (the heartbeat outlives slots; its age,
// not its absence, is the detection signal).
func (k *RedisKV) Set(ctx context.Context, key, value string) error {
	if err := k.rdb.Set(ctx, key, value, 0).Err(); err != nil {
		return fmt.Errorf("export: set %s: %w", key, err)
	}
	return nil
}

func randomToken() (string, error) {
	buf := make([]byte, lockTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("export: lock token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// FormatHeartbeat and ParseHeartbeat (un)encode the heartbeat value. The
// timestamp is stored as RFC 3339 with full precision.
func FormatHeartbeat(t time.Time) string { return t.UTC().Format(heartbeatLayout) }

// ParseHeartbeat decodes a heartbeat value; a malformed value is an error
// (treated as unknown by the caller).
func ParseHeartbeat(value string) (time.Time, error) {
	t, err := time.Parse(heartbeatLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("export: heartbeat value: %w", err)
	}
	return t, nil
}

// heartbeatAge is the 26 h rule's core arithmetic: how long ago the last
// successful run finished.
func heartbeatAge(now, last time.Time) time.Duration { return now.Sub(last) }
