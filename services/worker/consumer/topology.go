package consumer

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Production names (PRD §14.5).
const (
	EventsExchange        = "procurement.events"
	DLXExchange           = "procurement.dlx"
	DLQQueue              = "procurement.dlq"
	DocumentsHistoryQueue = "documents.history"
	NotificationsQueue    = "notifications"
)

// NotificationsRoutingKeys is the actor-facing subset of the §14.6 catalog:
// events a human recipient is notified about. documents.history receives
// the full catalog (bound with #). The subset is a documented convention
// (topology PR #95 pins the same list).
var NotificationsRoutingKeys = []string{
	"pr.submitted",
	"pr.approved",
	"pr.rejected",
	"po.issued",
	"po.acknowledged",
	"invoice.discrepancy",
}

// QueueSpec names one main consumer queue and the routing keys (event
// types, §14.5 routing key = event type) it is bound with on the events
// exchange.
type QueueSpec struct {
	Name     string
	Bindings []string
}

// Topology is the broker topology this consumer needs: everything the two
// #47 queues depend on. It is the consumer-scoped subset of the #46
// Topology (which also declares the exports queue), with identical
// declarations argument-for-argument — re-declaring after #45/#46 merge
// cannot raise PRECONDITION_FAILED. Default is production; integration
// tests derive isolated variants with Suffixed and short retry TTLs.
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

// Declare idempotently declares the topology on ch: both exchanges, the
// DLQ, every main queue with its bindings, and the retry ladder per main
// queue. Fails with a typed error naming the resource on the first problem.
func (t Topology) Declare(ch *amqp.Channel) error {
	if len(t.RetryTTLs) != MaxRetries {
		return fmt.Errorf("consumer: declare %s: need %d retry TTLs, got %d",
			t.EventsExchange, MaxRetries, len(t.RetryTTLs))
	}
	if err := ch.ExchangeDeclare(t.EventsExchange, "topic",
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("consumer: declare exchange %s: %w", t.EventsExchange, err)
	}
	if err := ch.ExchangeDeclare(t.DLXExchange, "topic",
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("consumer: declare exchange %s: %w", t.DLXExchange, err)
	}
	if _, err := ch.QueueDeclare(t.DLQQueue,
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("consumer: declare queue %s: %w", t.DLQQueue, err)
	}
	if err := ch.QueueBind(t.DLQQueue, "#", t.DLXExchange, false, nil); err != nil {
		return fmt.Errorf("consumer: bind %s to %s: %w", t.DLQQueue, t.DLXExchange, err)
	}
	for _, q := range t.Queues {
		if _, err := ch.QueueDeclare(q.Name,
			true, false, false, false, mainQueueArgs(t.DLXExchange)); err != nil {
			return fmt.Errorf("consumer: declare queue %s: %w", q.Name, err)
		}
		for _, key := range q.Bindings {
			if err := ch.QueueBind(q.Name, key, t.EventsExchange, false, nil); err != nil {
				return fmt.Errorf("consumer: bind %s with %s: %w", q.Name, key, err)
			}
		}
		// The TTL backoff ladder: each tier parks the message for its TTL,
		// then the broker dead-letters it back to this queue only.
		for attempt := 1; attempt <= MaxRetries; attempt++ {
			name := RetryQueueName(q.Name, attempt)
			if _, err := ch.QueueDeclare(name,
				true, false, false, false, retryQueueArgs(q.Name, t.RetryTTLs[attempt-1])); err != nil {
				return fmt.Errorf("consumer: declare queue %s: %w", name, err)
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
			return fmt.Errorf("consumer: delete queue %s: %w", name, err)
		}
	}
	if err := ch.ExchangeDelete(t.EventsExchange, false, false); err != nil {
		return fmt.Errorf("consumer: delete exchange %s: %w", t.EventsExchange, err)
	}
	if err := ch.ExchangeDelete(t.DLXExchange, false, false); err != nil {
		return fmt.Errorf("consumer: delete exchange %s: %w", t.DLXExchange, err)
	}
	return nil
}
