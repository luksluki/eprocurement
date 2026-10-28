package dlqmonitor

// Unit tests run broker-free (the depth read is observed through Sink and
// a warn-only failure path); the integration half at the bottom seeds a
// Suffixed DLQ on a real broker (TEST_RABBITMQ_URL; CI has no RabbitMQ
// service and is not changed — same skip pattern as the publisher and
// consumer suites).

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/services/worker/topology"
)

// The loop ticks immediately, then on the interval, and stops returning
// nil when ctx is cancelled. Readings reach Sink; depth 0 logs Info and
// depth > 0 logs Warn.
func TestRunTicksAndStops(t *testing.T) {
	var logs bytes.Buffer
	readings := make(chan int, 8)
	m := &Monitor{
		URL:      "unused (depthFn is stubbed through the loop below)",
		Interval: 15 * time.Millisecond,
		Logger:   slog.New(slog.NewJSONHandler(&logs, nil)),
		Sink:     func(depth int) { readings <- depth },
	}
	// Stub the broker read: alternate 0, 2, 0, 2… so both log levels fire.
	m.depthFn = func(ctx context.Context, url string, topo topology.Topology) (int, error) {
		select {
		case n := <-readings:
			return n, nil
		default:
			return 0, nil
		}
	}
	feed := []int{0, 2}
	for _, n := range feed {
		readings <- n
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(80 * time.Millisecond) // ≥ the immediate tick + several intervals
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	out := logs.String()
	if !bytes.Contains(logs.Bytes(), []byte(`"msg":"dlq depth summary","depth":0`)) {
		t.Errorf("no Info summary for depth 0 in logs: %s", out)
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"msg":"dlq depth summary","depth":2`)) {
		t.Errorf("no Warn summary for depth 2 in logs: %s", out)
	}
}

// A failing tick (broker down) is a warn, never fatal — the loop keeps
// running and still returns nil on cancel.
func TestRunSurvivesBrokerFailure(t *testing.T) {
	var logs bytes.Buffer
	m := &Monitor{
		URL:      "amqp://127.0.0.1:1/",
		Interval: 10 * time.Millisecond,
		Logger:   slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil (a down broker is non-fatal)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"msg":"dlq depth check failed"`)) {
		t.Errorf("no warn for the failed tick: %s", logs.String())
	}
}

// A zero Interval falls back to the PRD §16 hourly default.
func TestDefaultInterval(t *testing.T) {
	m := &Monitor{}
	if interval := m.interval(); interval != DefaultInterval {
		t.Fatalf("interval = %v, want %v", interval, DefaultInterval)
	}
	if DefaultInterval != time.Hour {
		t.Fatalf("DefaultInterval = %v, want 1h", DefaultInterval)
	}
}

// --- Integration (real RabbitMQ via TEST_RABBITMQ_URL) ---

type itEnv struct {
	amqpURL string
	topo    topology.Topology
}

func newItEnv(t *testing.T) *itEnv {
	t.Helper()
	amqpURL := os.Getenv("TEST_RABBITMQ_URL")
	if amqpURL == "" {
		t.Skip("TEST_RABBITMQ_URL not set; skipping integration test")
	}
	topo := topology.Default.Suffixed(fmt.Sprintf("-monit-%d", time.Now().UnixNano()))
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		t.Fatalf("dial rabbitmq: %v", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("test channel: %v", err)
	}
	if err := topo.Declare(ch); err != nil {
		t.Fatalf("declare topology: %v", err)
	}
	_ = ch.Close()
	t.Cleanup(func() {
		if ch, err := conn.Channel(); err == nil {
			_ = topo.Delete(ch)
			_ = ch.Close()
		}
		_ = conn.Close()
	})
	return &itEnv{amqpURL: amqpURL, topo: topo}
}

// seed publishes n messages straight to the DLX (the DLQ is bound # to
// it); the count is all Depth needs.
func (e *itEnv) seed(t *testing.T, n int) {
	t.Helper()
	conn, err := amqp.Dial(e.amqpURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := ch.PublishWithContext(context.Background(), e.topo.DLXExchange, "it.seed",
			false, false, amqp.Publishing{Body: []byte("x"), DeliveryMode: amqp.Persistent}); err != nil {
			t.Fatalf("seed publish: %v", err)
		}
	}
}

// AC (#48): the depth read sees a seeded DLQ (the count the QueueDeclare
// returns), and the loop surfaces it through Sink on its immediate tick
// and stops cleanly on cancel.
func TestDepthReadsSeededDLQ(t *testing.T) {
	e := newItEnv(t)
	e.seed(t, 2)

	got, err := depth(t.Context(), e.amqpURL, e.topo)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if got != 2 {
		t.Fatalf("depth = %d, want 2", got)
	}

	// The production-name Depth must at least answer on the shared broker
	// (no seeding of production queues — its exact count is whatever the
	// stack holds; the seeded-DLQ proof above is the deterministic one).
	if _, err := Depth(t.Context(), e.amqpURL); err != nil {
		t.Fatalf("Depth against production topology: %v", err)
	}

	var mu sync.Mutex
	var seen []int
	m := &Monitor{
		URL: e.amqpURL, Interval: 20 * time.Millisecond, Topology: e.topo,
		Sink: func(n int) { mu.Lock(); seen = append(seen, n); mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil on cancel", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("loop produced %d readings before cancel, want >= 2", len(seen))
	}
	for _, n := range seen {
		if n != 2 {
			t.Fatalf("readings = %v, want the seeded depth 2", seen)
		}
	}
}
