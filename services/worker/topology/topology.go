// Package topology declares the RabbitMQ topology of the procurement event
// backbone once (issue #46; PRD §14.5): the durable topic exchange
// procurement.events, the three durable consumer queues documents.history /
// notifications / exports (each dead-lettering to procurement.dlx), a TTL
// backoff retry ladder per queue (5 s → 30 s → 5 m, returning to its own
// queue through the default exchange), and the procurement.dlq final
// destination bound with # to procurement.dlx.
//
// Declare is idempotent and is called by the outbox publisher on every
// connect (boot and reconnect); the #47 consumers reuse the same declaration
// plus the NextRetry helper for their failure path.
//
// The DLX/DLQ declarations match the former PROVISIONAL bootstrap of #45
// argument-for-argument, so redeclaring on a broker that already carries the
// #45 pair cannot raise PRECONDITION_FAILED.
package topology

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Production names (PRD §14.5).
const (
	EventsExchange = "procurement.events"
	DLXExchange    = "procurement.dlx"
	DLQQueue       = "procurement.dlq"

	DocumentsHistoryQueue = "documents.history"
	NotificationsQueue    = "notifications"
	ExportsQueue          = "exports"
)

// Retry conventions shared by the topology and the #47 consumers.
const (
	// RetryCountHeader carries the number of retries already attempted.
	// Absent means first delivery.
	RetryCountHeader = "x-retry-count"

	// MaxRetries is the number of retry attempts after the first delivery;
	// the (MaxRetries+1)-th failure dead-letters to procurement.dlq.
	MaxRetries = 3
)

// DefaultRetryTTLs is the PRD §14.5 backoff ladder: 5 s → 30 s → 5 min.
var DefaultRetryTTLs = []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}

// NotificationsRoutingKeys is the actor-facing subset of the §14.6 catalog:
// events a human recipient is notified about. The full catalog flows to
// documents.history (bound with #); the subset is a documented convention,
// open to maintainer override in review.
var NotificationsRoutingKeys = []string{
	"pr.submitted",
	"pr.approved",
	"pr.rejected",
	"po.issued",
	"po.acknowledged",
	"invoice.discrepancy",
}

// QueueSpec names one main consumer queue and the routing keys (event types,
// §14.5 routing key = event type) it is bound with on the events exchange.
type QueueSpec struct {
	Name     string
	Bindings []string
}

// Topology is the parameterized broker topology. Default is production;
// integration tests derive isolated variants with Suffixed and short TTLs.
type Topology struct {
	EventsExchange string
	DLXExchange    string
	DLQQueue       string
	Queues         []QueueSpec
	RetryTTLs      []time.Duration // one per retry tier; len must be MaxRetries
}

// Default is the production topology (PRD §14.5).
var Default = Topology{
	EventsExchange: EventsExchange,
	DLXExchange:    DLXExchange,
	DLQQueue:       DLQQueue,
	Queues: []QueueSpec{
		{Name: DocumentsHistoryQueue, Bindings: []string{"#"}},
		{Name: NotificationsQueue, Bindings: append([]string(nil), NotificationsRoutingKeys...)},
		{Name: ExportsQueue, Bindings: []string{"export.requested"}},
	},
	RetryTTLs: append([]time.Duration(nil), DefaultRetryTTLs...),
}

// Suffixed returns a copy with suffix appended to every exchange and queue
// name (retry queue names derive from main names, so they follow). Bindings
// and TTLs are shared by value; callers mutate the copy's RetryTTLs freely.
func (t Topology) Suffixed(suffix string) Topology {
	clone := Topology{
		EventsExchange: t.EventsExchange + suffix,
		DLXExchange:    t.DLXExchange + suffix,
		DLQQueue:       t.DLQQueue + suffix,
		RetryTTLs:      append([]time.Duration(nil), t.RetryTTLs...),
	}
	for _, q := range t.Queues {
		clone.Queues = append(clone.Queues, QueueSpec{
			Name:     q.Name + suffix,
			Bindings: append([]string(nil), q.Bindings...),
		})
	}
	return clone
}

// RetryQueueName is the retry-tier queue for a main queue (attempt is
// 1-based: 1 = the 5 s tier, 2 = 30 s, 3 = 5 min).
func RetryQueueName(queue string, attempt int) string {
	return fmt.Sprintf("%s.retry.%d", queue, attempt)
}

// NextRetry decides the failure path of one delivery from queue: the retry
// queue of tier retryCount+1 while the attempt budget lasts, ok=false when it
// is exhausted — then the caller rejects the delivery without requeue so the
// broker dead-letters it to the DLX and on to procurement.dlq. retryCount is
// the value of the RetryCountHeader (0 on first delivery), read tolerantly
// with RetryCount.
func NextRetry(queue string, retryCount int32) (string, bool) {
	if retryCount >= MaxRetries || retryCount < 0 {
		return "", false
	}
	return RetryQueueName(queue, int(retryCount)+1), true
}

// RetryCount reads the RetryCountHeader tolerantly: 0 when absent, and any
// integer width AMQP field tables decode to (clients encode small integers
// differently) when present.
func RetryCount(h amqp.Table) int32 {
	switch v := h[RetryCountHeader].(type) {
	case int8:
		return int32(v)
	case int16:
		return int32(v)
	case int32:
		return v
	case int64:
		return int32(v)
	case int:
		return int32(v)
	default:
		return 0
	}
}

// mainQueueArgs dead-letters a failed delivery of a main queue to the DLX
// (PRD §14.5), from where the DLQ's # binding picks it up.
func mainQueueArgs(dlExchange string) amqp.Table {
	return amqp.Table{"x-dead-letter-exchange": dlExchange}
}

// retryQueueArgs parks a message for ttl, then dead-letters it back to its
// main queue through the DEFAULT exchange with the main queue's name as the
// routing key — so one queue's retry never lands in another queue.
func retryQueueArgs(mainQueue string, ttl time.Duration) amqp.Table {
	return amqp.Table{
		"x-message-ttl":             ttl.Milliseconds(),
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": mainQueue,
	}
}

// Declare idempotently declares the full topology on ch: both exchanges, the
// DLQ, every main queue with its bindings, and the retry ladder per main
// queue. Fails with a typed error naming the resource on the first problem.
func (t Topology) Declare(ch *amqp.Channel) error {
	if len(t.RetryTTLs) != MaxRetries {
		return fmt.Errorf("topology: declare %s: need %d retry TTLs, got %d",
			t.EventsExchange, MaxRetries, len(t.RetryTTLs))
	}
	if err := ch.ExchangeDeclare(t.EventsExchange, "topic",
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("topology: declare exchange %s: %w", t.EventsExchange, err)
	}
	if err := ch.ExchangeDeclare(t.DLXExchange, "topic",
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("topology: declare exchange %s: %w", t.DLXExchange, err)
	}
	if _, err := ch.QueueDeclare(t.DLQQueue,
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("topology: declare queue %s: %w", t.DLQQueue, err)
	}
	if err := ch.QueueBind(t.DLQQueue, "#", t.DLXExchange, false, nil); err != nil {
		return fmt.Errorf("topology: bind %s to %s: %w", t.DLQQueue, t.DLXExchange, err)
	}
	for _, q := range t.Queues {
		if _, err := ch.QueueDeclare(q.Name,
			true, false, false, false, mainQueueArgs(t.DLXExchange)); err != nil {
			return fmt.Errorf("topology: declare queue %s: %w", q.Name, err)
		}
		for _, key := range q.Bindings {
			if err := ch.QueueBind(q.Name, key, t.EventsExchange, false, nil); err != nil {
				return fmt.Errorf("topology: bind %s with %s: %w", q.Name, key, err)
			}
		}
		// The TTL backoff ladder: each tier parks the message for its TTL,
		// then the broker dead-letters it back to this queue only.
		for attempt := 1; attempt <= MaxRetries; attempt++ {
			name := RetryQueueName(q.Name, attempt)
			if _, err := ch.QueueDeclare(name,
				true, false, false, false, retryQueueArgs(q.Name, t.RetryTTLs[attempt-1])); err != nil {
				return fmt.Errorf("topology: declare queue %s: %w", name, err)
			}
		}
	}
	return nil
}

// QueueNames lists every queue the topology declares: mains first (spec
// order), then each main's retry ladder, then the DLQ. Passive checks and
// test cleanup walk this list.
func (t Topology) QueueNames() []string {
	names := make([]string, 0, len(t.Queues)*(MaxRetries+1)+1)
	for _, q := range t.Queues {
		names = append(names, q.Name)
		for attempt := 1; attempt <= MaxRetries; attempt++ {
			names = append(names, RetryQueueName(q.Name, attempt))
		}
	}
	return append(names, t.DLQQueue)
}

// Delete removes every queue and both exchanges the topology declared. Test
// cleanup only — production never deletes broker topology.
func (t Topology) Delete(ch *amqp.Channel) error {
	for _, name := range t.QueueNames() {
		if _, err := ch.QueueDelete(name, false, false, false); err != nil {
			return fmt.Errorf("topology: delete queue %s: %w", name, err)
		}
	}
	if err := ch.ExchangeDelete(t.EventsExchange, false, false); err != nil {
		return fmt.Errorf("topology: delete exchange %s: %w", t.EventsExchange, err)
	}
	if err := ch.ExchangeDelete(t.DLXExchange, false, false); err != nil {
		return fmt.Errorf("topology: delete exchange %s: %w", t.DLXExchange, err)
	}
	return nil
}
