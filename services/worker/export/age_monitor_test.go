package export

// Unit tests for the #55 export-age refresher (age_monitor.go): the
// happy path sets per-tenant ages from the heartbeat; absent, malformed,
// and unreadable heartbeats delete the series (the honest-gap semantics);
// tenants that leave the enumeration lose their series; an enumeration
// failure keeps the previous values; the loop stops returning nil on
// cancel.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeAgeSink records Set/Delete calls (export_age_seconds semantics).
type fakeAgeSink struct {
	mu     sync.Mutex
	ages   map[string]time.Duration
	calls  []string
	onCall func(op, tenant string)
}

func newFakeAgeSink() *fakeAgeSink { return &fakeAgeSink{ages: map[string]time.Duration{}} }

func (f *fakeAgeSink) SetExportAge(tenantID string, age time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ages[tenantID] = age
	f.calls = append(f.calls, "set:"+tenantID)
}

func (f *fakeAgeSink) DeleteExportAge(tenantID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ages, tenantID)
	f.calls = append(f.calls, "delete:"+tenantID)
}

// errKV wraps fakeKV failing every Get (the "heartbeat unreadable" arm).
type errKV struct{ *fakeKV }

func (e errKV) Get(context.Context, string) (string, bool, error) {
	return "", false, errors.New("redis down")
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAgeMonitorTickTable(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		kv      KV
		tenants []string
		want    map[string]time.Duration // tenant -> expected age; missing = no series
	}{
		{
			name: "valid heartbeats set the age in seconds",
			kv: func() KV {
				kv := newFakeKV()
				kv.values[HeartbeatKey("t-fresh")] = FormatHeartbeat(now.Add(-2 * time.Hour))
				kv.values[HeartbeatKey("t-stale")] = FormatHeartbeat(now.Add(-30 * time.Hour))
				return kv
			}(),
			tenants: []string{"t-fresh", "t-stale"},
			want:    map[string]time.Duration{"t-fresh": 2 * time.Hour, "t-stale": 30 * time.Hour},
		},
		{
			name:    "absent heartbeat deletes the series (no fake 0)",
			kv:      newFakeKV(),
			tenants: []string{"t-never"},
			want:    map[string]time.Duration{},
		},
		{
			name: "malformed heartbeat deletes the series",
			kv: func() KV {
				kv := newFakeKV()
				kv.values[HeartbeatKey("t-bad")] = "not-a-timestamp"
				return kv
			}(),
			tenants: []string{"t-bad"},
			want:    map[string]time.Duration{},
		},
		{
			name:    "unreadable heartbeat (redis error) deletes the series",
			kv:      errKV{newFakeKV()},
			tenants: []string{"t-redis-down"},
			want:    map[string]time.Duration{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newFakeAgeSink()
			a := NewAgeMonitor(&fakeTenants{tenants: tc.tenants}, tc.kv, sink, time.Hour, quietLogger())
			a.now = func() time.Time { return now }
			a.tick(context.Background())

			if len(sink.ages) != len(tc.want) {
				t.Fatalf("series = %v, want %v", sink.ages, tc.want)
			}
			for tenant, wantAge := range tc.want {
				got, ok := sink.ages[tenant]
				if !ok {
					t.Fatalf("tenant %s has no series, want age %s", tenant, wantAge)
				}
				if got != wantAge {
					t.Fatalf("age[%s] = %s, want %s", tenant, got, wantAge)
				}
			}
		})
	}
}

// A tenant that leaves the enumeration must not keep a frozen series —
// that would be permanent fake staleness on the dashboard.
func TestAgeMonitorRemovesDepartedTenants(t *testing.T) {
	kv := newFakeKV()
	kv.values[HeartbeatKey("t-a")] = FormatHeartbeat(time.Now())
	kv.values[HeartbeatKey("t-gone")] = FormatHeartbeat(time.Now())
	lister := &fakeTenants{tenants: []string{"t-a", "t-gone"}}
	sink := newFakeAgeSink()
	a := NewAgeMonitor(lister, kv, sink, time.Hour, quietLogger())
	a.tick(context.Background())
	if _, ok := sink.ages["t-gone"]; !ok {
		t.Fatal("t-gone should have a series before departing")
	}

	lister.tenants = []string{"t-a"} // t-gone no longer enumerated
	a.tick(context.Background())
	if _, ok := sink.ages["t-gone"]; ok {
		t.Fatalf("departed tenant kept a series: %v", sink.ages)
	}
	if _, ok := sink.ages["t-a"]; !ok {
		t.Fatal("remaining tenant lost its series")
	}
}

// An enumeration failure (register unreadable) keeps the previous values —
// stale-but-real beats a wipe or a fake refresh.
func TestAgeMonitorKeepsValuesOnEnumerationFailure(t *testing.T) {
	kv := newFakeKV()
	kv.values[HeartbeatKey("t-1")] = FormatHeartbeat(time.Now())
	sink := newFakeAgeSink()
	a := NewAgeMonitor(&fakeTenants{tenants: []string{"t-1"}}, kv, sink, time.Hour, quietLogger())
	a.tick(context.Background())
	if len(sink.ages) != 1 {
		t.Fatalf("series after first tick = %v, want t-1", sink.ages)
	}

	a.Tenants = &fakeTenants{err: errors.New("postgres down")}
	callsBefore := len(sink.calls)
	a.tick(context.Background())
	if len(sink.calls) != callsBefore {
		t.Fatalf("sink touched on enumeration failure: %v", sink.calls)
	}
	if _, ok := sink.ages["t-1"]; !ok {
		t.Fatal("enumeration failure wiped a real series")
	}
}

// Run ticks immediately then on the interval, and stops returning nil on
// cancel (the runComponents contract).
func TestAgeMonitorRunLifecycle(t *testing.T) {
	kv := newFakeKV()
	kv.values[HeartbeatKey("t-1")] = FormatHeartbeat(time.Now())
	sink := newFakeAgeSink()
	a := NewAgeMonitor(&fakeTenants{tenants: []string{"t-1"}}, kv, sink, 15*time.Millisecond, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		sink.mu.Lock()
		n := len(sink.calls)
		sink.mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.calls) < 3 {
		t.Fatalf("ticks = %d, want >= 3 (immediate tick + interval ticks)", len(sink.calls))
	}
}

// The interval default is the documented 60 s.
func TestAgeMonitorDefaultInterval(t *testing.T) {
	if DefaultAgeInterval != 60*time.Second {
		t.Fatalf("DefaultAgeInterval = %v, want 60s", DefaultAgeInterval)
	}
	a := NewAgeMonitor(&fakeTenants{}, newFakeKV(), newFakeAgeSink(), 0, quietLogger())
	if a.Interval != DefaultAgeInterval {
		t.Fatalf("interval = %v, want the default %v", a.Interval, DefaultAgeInterval)
	}
}
