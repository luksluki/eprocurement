// Package ratelimit provides a small in-memory token-bucket limiter.
//
// One bucket per key, refilled continuously at rate tokens/second up to burst.
// State is per-process: sufficient for the single-instance login limiter the
// identity service needs today; the gateway adds a shared limiter later.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is safe for concurrent use.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	mu    sync.Mutex
	seen  map[string]*bucket
	now   func() time.Time
}

// New returns a Limiter granting burst events immediately, then rate events
// per second.
func New(rate float64, burst int) *Limiter {
	return &Limiter{
		rate:  rate,
		burst: float64(burst),
		seen:  make(map[string]*bucket),
		now:   time.Now,
	}
}

// Allow consumes one token for key and reports whether it was granted.
func (l *Limiter) Allow(key string) bool {
	return l.allowAt(l.now(), key)
}

func (l *Limiter) allowAt(now time.Time, key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.seen[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.seen[key] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
