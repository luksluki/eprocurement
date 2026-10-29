// Package dlqmonitor is the worker's half of the PRD §16 "DLQ monitor —
// hourly summary metric + admin endpoint" (issue #48): a loop that reads
// procurement.dlq's ready-message depth every interval and logs a
// structured summary line per tick — the interim metric surface. The
// Prometheus/OTel export of the same depth lands with the observability
// stack (#54, PRD §18); the admin endpoint half of the bullet lives in the
// gateway (services/gateway/dlq).
//
// The monitor adds no boot-order coupling: it never dials at boot, and a
// broker that is down costs one warn line per tick, nothing more.
package dlqmonitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/services/worker/topology"
)

// dialTimeout bounds one AMQP dial attempt (the worker's convention).
const dialTimeout = 10 * time.Second

// DefaultInterval is the PRD §16 cadence: one summary per hour. The env
// override (DLQ_SUMMARY_INTERVAL) exists for tests and operators.
const DefaultInterval = time.Hour

// Depth reports the production DLQ's ready-message count with one
// short-lived connection: dial, bootstrap the DLX/DLQ declarations
// (argument-identical to topology.Declare, so the read also works on a
// broker whose topology is not declared yet), and close.
func Depth(ctx context.Context, url string) (int, error) {
	return depth(ctx, url, topology.Default)
}

// depth is Depth against an explicit topology (tests pass Suffixed
// variants for broker isolation).
func depth(ctx context.Context, url string, topo topology.Topology) (int, error) {
	conn, err := amqp.DialConfig(url, amqp.Config{
		Dial: (&net.Dialer{Timeout: dialTimeout}).Dial,
	})
	if err != nil {
		return 0, fmt.Errorf("dlqmonitor: dial rabbitmq: %w", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		return 0, fmt.Errorf("dlqmonitor: open channel: %w", err)
	}
	q, err := declareDLQ(ch, topo)
	if err != nil {
		return 0, err
	}
	return q.Messages, nil
}

// declareDLQ declares the DLX exchange, the DLQ queue, and the # binding —
// argument-identical to the same three calls inside topology.Declare (the
// gateway's services/gateway/dlq declares the identical triple), so a
// broker that already carries the #46 topology accepts it without
// PRECONDITION_FAILED. The declare doubles as the depth read: its return
// value counts the queue's ready messages.
func declareDLQ(ch *amqp.Channel, topo topology.Topology) (amqp.Queue, error) {
	if err := ch.ExchangeDeclare(topo.DLXExchange, "topic",
		true, false, false, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("dlqmonitor: declare exchange %s: %w", topo.DLXExchange, err)
	}
	q, err := ch.QueueDeclare(topo.DLQQueue,
		true, false, false, false, nil)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("dlqmonitor: declare queue %s: %w", topo.DLQQueue, err)
	}
	if err := ch.QueueBind(topo.DLQQueue, "#", topo.DLXExchange, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("dlqmonitor: bind %s to %s: %w", topo.DLQQueue, topo.DLXExchange, err)
	}
	return q, nil
}

// Monitor is the recurring depth summary. URL and Interval come from
// config; a zero Interval means DefaultInterval. Topology is injectable
// for tests (zero means topology.Default); Sink receives every successful
// reading so tests can observe ticks without parsing logs.
type Monitor struct {
	URL      string
	Interval time.Duration
	Topology topology.Topology
	Logger   *slog.Logger
	Sink     func(depth int)

	// depthFn is the broker read one tick performs; tests stub it to run
	// the loop broker-free. nil means the package's depth function.
	depthFn func(ctx context.Context, url string, topo topology.Topology) (int, error)
}

// Run ticks immediately, then every Interval, until ctx is cancelled —
// then returns nil. A failed tick (broker down) is a warn, never fatal:
// the monitor must not add a boot-order or runtime dependency on the
// broker (PRD §14.8 "worker down").
func (m *Monitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.interval())
	defer ticker.Stop()
	m.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

// interval is Interval or the PRD §16 hourly default.
func (m *Monitor) interval() time.Duration {
	if m.Interval > 0 {
		return m.Interval
	}
	return DefaultInterval
}

// tick reads the depth once and logs the structured summary line: depth 0
// is Info (the healthy state), anything above is Warn. The alerting
// thresholds layered on that cadence live in docs/ops/dlq-monitor.md.
func (m *Monitor) tick(ctx context.Context) {
	read := depth
	if m.depthFn != nil {
		read = m.depthFn
	}
	topo := m.Topology
	if topo.DLXExchange == "" && topo.DLQQueue == "" {
		topo = topology.Default
	}
	n, err := read(ctx, m.URL, topo)
	if err != nil {
		if ctx.Err() == nil { // shutting down is not a broker problem
			m.logger().Warn("dlq depth check failed", "error", err)
		}
		return
	}
	if m.Sink != nil {
		m.Sink(n)
	}
	if n == 0 {
		m.logger().Info("dlq depth summary", "depth", 0)
		return
	}
	m.logger().Warn("dlq depth summary", "depth", n)
}

func (m *Monitor) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
