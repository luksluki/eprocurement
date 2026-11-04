package ratelimit

import (
	"testing"
	"time"
)

func TestBurstThenDeny(t *testing.T) {
	l := New(1.0, 3) // 1/s, burst 3
	for i := 0; i < 3; i++ {
		if !l.Allow("k") {
			t.Fatalf("attempt %d should be allowed within burst", i+1)
		}
	}
	if l.Allow("k") {
		t.Fatal("4th immediate attempt should be denied")
	}
}

func TestRefillOverTime(t *testing.T) {
	l := New(1.0, 1)
	base := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return base }

	if !l.Allow("k") {
		t.Fatal("first attempt should be allowed")
	}
	if l.Allow("k") {
		t.Fatal("second immediate attempt should be denied")
	}

	base = base.Add(1500 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("attempt after refill window should be allowed")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(0.001, 1)
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("different keys must have independent buckets")
	}
}
