package export

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisEnv connects to TEST_REDIS_URL and skips the test when unset — the
// #47 convention for broker-gated integration tests (CI has no Redis
// service; adding one is a named follow-up, not part of #49).
func redisEnv(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping integration test")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse TEST_REDIS_URL: %v", err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	pingCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return client
}

// TestRedisLockAcquireRelease pins the lock lifecycle against a real
// Redis: acquisition is exclusive, the holder's release frees the key, and
// a wrong-token release (a lock lost to TTL and re-acquired by someone
// else) never deletes the other holder's lock.
func TestRedisLockAcquireRelease(t *testing.T) {
	rdb := redisEnv(t)
	kv := NewRedisKV(rdb)
	ctx := t.Context()
	key := LockKey("t-locktest")

	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	token, acquired, err := kv.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first Acquire = (%q, %v, %v), want acquired", token, acquired, err)
	}
	if len(token) != 2*lockTokenBytes {
		t.Fatalf("token %q is not %d hex chars", token, 2*lockTokenBytes)
	}

	// Second acquisition while held: refused, no error.
	_, acquired, err = kv.Acquire(ctx, key, time.Minute)
	if err != nil || acquired {
		t.Fatalf("second Acquire = (%v, %v), want refused", acquired, err)
	}

	// A foreign token must not release the lock (compare-and-delete).
	if err := kv.Release(ctx, key, "not-the-token"); err != nil {
		t.Fatalf("foreign Release: %v", err)
	}
	if _, acquired, err = kv.Acquire(ctx, key, time.Minute); err != nil || acquired {
		t.Fatalf("Acquire after foreign release = (%v, %v), still held — compare-and-del broken", acquired, err)
	}

	// The holder's release frees the lock for the next acquirer.
	if err := kv.Release(ctx, key, token); err != nil {
		t.Fatalf("holder Release: %v", err)
	}
	token2, acquired, err := kv.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after release = (%v, %v), want acquired", acquired, err)
	}
	if token2 == token {
		t.Fatal("tokens must be random per acquisition")
	}

	// TTL bounds a crashed holder: the key expires and can be re-acquired.
	ttl := 150 * time.Millisecond
	if err := kv.Release(ctx, key, token2); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, acquired, _ := kv.Acquire(ctx, key, ttl); !acquired {
		t.Fatal("setup Acquire failed")
	}
	time.Sleep(250 * time.Millisecond)
	_, acquired, err = kv.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after TTL expiry = (%v, %v), want acquired", acquired, err)
	}
	_ = rdb.Del(ctx, key)
}

// TestRedisLockTwoParallelAcquirers is the overlap AC: two schedulers race
// for the same tenant's lock — exactly one wins, the loser is told
// acquired=false (its caller skips and logs, never records a run).
func TestRedisLockTwoParallelAcquirers(t *testing.T) {
	rdb := redisEnv(t)
	kv := NewRedisKV(rdb)
	ctx := t.Context()
	key := LockKey("t-lockrace")
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	const racers = 2
	var winners atomic.Int32
	var mu sync.Mutex
	tokens := map[string]bool{}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			token, acquired, err := kv.Acquire(ctx, key, time.Minute)
			if err != nil {
				t.Errorf("racer %d Acquire error: %v", i, err)
				return
			}
			if acquired {
				winners.Add(1)
				mu.Lock()
				tokens[token] = true
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners.Load() != 1 {
		t.Fatalf("winners = %d, want exactly 1 of %d racers", winners.Load(), racers)
	}
	// The winner's lock is really in Redis with its token.
	stored, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("GET lock: %v", err)
	}
	if !tokens[stored] {
		t.Fatalf("stored token %q does not belong to the winner", stored)
	}
	_ = rdb.Del(ctx, key)
}

// TestRedisHeartbeatRoundtrip pins the heartbeat encoding on a real Redis:
// Set stores, Get reads back, absent maps to ok=false.
func TestRedisHeartbeatRoundtrip(t *testing.T) {
	rdb := redisEnv(t)
	kv := NewRedisKV(rdb)
	ctx := t.Context()
	key := HeartbeatKey("t-hbtest")
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if value, ok, err := kv.Get(ctx, key); err != nil || ok || value != "" {
		t.Fatalf("absent Get = (%q, %v, %v), want ok=false", value, ok, err)
	}

	last := time.Date(2026, 10, 26, 1, 2, 3, 456789000, time.UTC)
	if err := kv.Set(ctx, key, FormatHeartbeat(last)); err != nil {
		t.Fatalf("Set heartbeat: %v", err)
	}
	value, ok, err := kv.Get(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Get heartbeat = (%v, %v)", ok, err)
	}
	got, err := ParseHeartbeat(value)
	if err != nil {
		t.Fatalf("ParseHeartbeat: %v", err)
	}
	if heartbeatAge(time.Date(2026, 10, 27, 1, 2, 3, 456789000, time.UTC), got) != 24*time.Hour {
		t.Fatalf("age = %s, want 24h", heartbeatAge(time.Date(2026, 10, 27, 1, 2, 3, 456789000, time.UTC), got))
	}
	if _, err := ParseHeartbeat("garbage"); err == nil {
		t.Fatal("ParseHeartbeat(garbage) = nil error")
	}
	_ = rdb.Del(ctx, key)
}
