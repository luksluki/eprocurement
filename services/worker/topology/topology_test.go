package topology

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The production topology is exactly PRD §14.5: names, bindings, TTL ladder.
func TestDefaultTopology(t *testing.T) {
	if Default.EventsExchange != "procurement.events" ||
		Default.DLXExchange != "procurement.dlx" || Default.DLQQueue != "procurement.dlq" {
		t.Fatalf("production names = %s/%s/%s, want procurement.events/procurement.dlx/procurement.dlq",
			Default.EventsExchange, Default.DLXExchange, Default.DLQQueue)
	}
	want := []struct {
		name     string
		bindings []string
	}{
		{DocumentsHistoryQueue, []string{"#"}},
		{NotificationsQueue, append([]string(nil), NotificationsRoutingKeys...)},
		{ExportsQueue, []string{"export.requested"}},
	}
	if len(Default.Queues) != len(want) {
		t.Fatalf("queues = %d, want %d", len(Default.Queues), len(want))
	}
	for i, q := range Default.Queues {
		if q.Name != want[i].name {
			t.Fatalf("queue[%d] = %s, want %s", i, q.Name, want[i].name)
		}
		if fmt.Sprint(q.Bindings) != fmt.Sprint(want[i].bindings) {
			t.Fatalf("%s bindings = %v, want %v", q.Name, q.Bindings, want[i].bindings)
		}
	}
	wantTTLs := []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}
	if fmt.Sprint(Default.RetryTTLs) != fmt.Sprint(wantTTLs) {
		t.Fatalf("retry TTLs = %v, want %v (5 s → 30 s → 5 min)", Default.RetryTTLs, wantTTLs)
	}
}

// AC (failed message lands in DLQ after max attempts): the failure-path
// decision table — 3 retries, then the DLX on the fourth failure. A negative
// or corrupt count also dead-letters rather than looping forever.
func TestNextRetry(t *testing.T) {
	cases := []struct {
		name   string
		count  int32
		wantQ  string
		wantOK bool
	}{
		{"first failure retries at tier 1", 0, "exports.retry.1", true},
		{"second failure retries at tier 2", 1, "exports.retry.2", true},
		{"third failure retries at tier 3", 2, "exports.retry.3", true},
		{"fourth failure dead-letters", 3, "", false},
		{"far beyond the budget dead-letters", 9, "", false},
		{"negative (corrupt) count dead-letters", -1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotQ, gotOK := NextRetry("exports", tc.count)
			if gotQ != tc.wantQ || gotOK != tc.wantOK {
				t.Fatalf("NextRetry(exports, %d) = (%q, %v), want (%q, %v)",
					tc.count, gotQ, gotOK, tc.wantQ, tc.wantOK)
			}
		})
	}
}

// RetryCount reads the header tolerantly: absent → 0, any AMQP integer width
// → the value, anything else → 0.
func TestRetryCount(t *testing.T) {
	cases := []struct {
		name string
		hdr  amqp.Table
		want int32
	}{
		{"absent", amqp.Table{}, 0},
		{"int32", amqp.Table{RetryCountHeader: int32(2)}, 2},
		{"int64", amqp.Table{RetryCountHeader: int64(3)}, 3},
		{"int", amqp.Table{RetryCountHeader: 1}, 1},
		{"int16", amqp.Table{RetryCountHeader: int16(1)}, 1},
		{"non-integer", amqp.Table{RetryCountHeader: "two"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryCount(tc.hdr); got != tc.want {
				t.Fatalf("RetryCount(%v) = %d, want %d", tc.hdr, got, tc.want)
			}
		})
	}
}

// Queue arguments are the §14.5 contract; they must match argument-for-
// argument what a redeclare (reconnect path) sends, or the broker answers
// PRECONDITION_FAILED.
func TestQueueArgs(t *testing.T) {
	main := mainQueueArgs("procurement.dlx")
	if len(main) != 1 || main["x-dead-letter-exchange"] != "procurement.dlx" {
		t.Fatalf("main queue args = %v, want exactly x-dead-letter-exchange=procurement.dlx", main)
	}
	retry := retryQueueArgs("exports", 30*time.Second)
	want := amqp.Table{
		"x-message-ttl":             int64(30000),
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": "exports",
	}
	if len(retry) != len(want) {
		t.Fatalf("retry queue args = %v, want %v", retry, want)
	}
	for k, v := range want {
		if retry[k] != v {
			t.Fatalf("retry queue args[%q] = %v (%T), want %v (%T)", k, retry[k], retry[k], v, v)
		}
	}
}

// The retry ladder is per main queue and 1-based.
func TestRetryQueueName(t *testing.T) {
	if got := RetryQueueName("documents.history", 1); got != "documents.history.retry.1" {
		t.Fatalf("RetryQueueName = %q, want documents.history.retry.1", got)
	}
}

// Suffixed renames everything (retry names follow), copies TTLs by value,
// and keeps the original untouched — the isolation basis of the broker tests.
func TestSuffixed(t *testing.T) {
	clone := Default.Suffixed(".x1")
	clone.RetryTTLs[0] = time.Millisecond
	if clone.EventsExchange != "procurement.events.x1" ||
		clone.DLXExchange != "procurement.dlx.x1" || clone.DLQQueue != "procurement.dlq.x1" {
		t.Fatalf("suffixed names = %s/%s/%s", clone.EventsExchange, clone.DLXExchange, clone.DLQQueue)
	}
	if clone.Queues[0].Name != "documents.history.x1" ||
		clone.Queues[2].Name != "exports.x1" || len(clone.Queues) != len(Default.Queues) {
		t.Fatalf("suffixed queues wrong: %v", clone.Queues)
	}
	if fmt.Sprint(clone.Queues[1].Bindings) != fmt.Sprint(Default.Queues[1].Bindings) {
		t.Fatalf("suffixed bindings changed: %v", clone.Queues[1].Bindings)
	}
	if Default.RetryTTLs[0] != 5*time.Second {
		t.Fatalf("mutating the clone's TTLs touched Default: %v", Default.RetryTTLs)
	}
	if got := RetryQueueName(clone.Queues[2].Name, 2); got != "exports.x1.retry.2" {
		t.Fatalf("retry name under suffix = %q", got)
	}
}

// QueueNames walks mains, their retry ladders, then the DLQ — the passive
// check and cleanup inventory.
func TestQueueNames(t *testing.T) {
	got := Default.Suffixed(".q").QueueNames()
	want := []string{
		"documents.history.q", "documents.history.q.retry.1", "documents.history.q.retry.2", "documents.history.q.retry.3",
		"notifications.q", "notifications.q.retry.1", "notifications.q.retry.2", "notifications.q.retry.3",
		"exports.q", "exports.q.retry.1", "exports.q.retry.2", "exports.q.retry.3",
		"procurement.dlq.q",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("QueueNames =\n%v\nwant\n%v", got, want)
	}
}

// Failure path: a topology without exactly MaxRetries TTLs refuses to
// declare (nil channel is safe — the guard runs before any channel use).
func TestDeclareRejectsBadRetryTTLs(t *testing.T) {
	topo := Default.Suffixed(".bad")
	topo.RetryTTLs = nil
	if err := topo.Declare(nil); err == nil {
		t.Fatal("declare without retry TTLs succeeded, want error")
	}
	topo.RetryTTLs = []time.Duration{time.Second, time.Second}
	if err := topo.Declare(nil); err == nil {
		t.Fatal("declare with 2 retry TTLs succeeded, want error")
	}
}

// --- Broker-backed tests (real RabbitMQ via TEST_RABBITMQ_URL; CI has no
// broker and skips — the same harness shape as the publisher suite). ---

func openAMQP(t *testing.T) *amqp.Channel {
	t.Helper()
	amqpURL := os.Getenv("TEST_RABBITMQ_URL")
	if amqpURL == "" {
		t.Skip("TEST_RABBITMQ_URL not set; skipping integration test")
	}
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		t.Fatalf("dial rabbitmq: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	return ch
}

// isolatedTopology declares a fully renamed clone of the production topology
// under suffix (own exchanges and queues, deleted on cleanup) so tests on
// the shared dev broker cannot see or leave anything. ttls overrides the
// retry ladder for the short-TTL cycle tests.
func isolatedTopology(t *testing.T, ch *amqp.Channel, suffix string, ttls []time.Duration) Topology {
	t.Helper()
	topo := Default.Suffixed(suffix)
	if ttls != nil {
		topo.RetryTTLs = ttls
	}
	if err := topo.Declare(ch); err != nil {
		t.Fatalf("declare topology: %v", err)
	}
	t.Cleanup(func() {
		if err := topo.Delete(ch); err != nil {
			t.Fatalf("cleanup topology: %v", err)
		}
	})
	return topo
}

func publish(t *testing.T, ch *amqp.Channel, exchange, key, id string, headers amqp.Table) {
	t.Helper()
	err := ch.PublishWithContext(t.Context(), exchange, key, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    id,
		Timestamp:    time.Now().UTC(),
		Headers:      headers,
		Body:         []byte(`{"event_id":"` + id + `"}`),
	})
	if err != nil {
		t.Fatalf("publish %s: %v", id, err)
	}
}

// fetchN waits for want deliveries on queue (acking them), fails on timeout,
// and proves the queue holds nothing beyond them.
func fetchN(t *testing.T, ch *amqp.Channel, queue string, want int) []amqp.Delivery {
	t.Helper()
	var got []amqp.Delivery
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(got) < want {
		msg, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatalf("basic.get %s: %v", queue, err)
		}
		if ok {
			got = append(got, msg)
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) < want {
		t.Fatalf("queue %s: got %d messages, want %d", queue, len(got), want)
	}
	if extra, ok, _ := ch.Get(queue, true); ok {
		t.Fatalf("queue %s: unexpected extra message %q — routing leaked", queue, extra.MessageId)
	}
	return got
}

// drainKeys returns the sorted routing keys of exactly want messages.
func drainKeys(t *testing.T, ch *amqp.Channel, queue string, want int) []string {
	t.Helper()
	msgs := fetchN(t, ch, queue, want)
	keys := make([]string, 0, len(msgs))
	for _, m := range msgs {
		keys = append(keys, m.RoutingKey)
	}
	sort.Strings(keys)
	return keys
}

// assertEmpty proves a queue holds nothing (retry tiers / DLQ after tests
// that never fail a delivery).
func assertEmpty(t *testing.T, ch *amqp.Channel, queue string) {
	t.Helper()
	if msg, ok, err := ch.Get(queue, true); err != nil || ok {
		t.Fatalf("queue %s not empty: (%q, %v)", queue, msg.MessageId, err)
	}
}

// AC (message routes to the right queue): representative §14.6 event types
// published to the topic exchange arrive exactly where the binding table
// says — documents.history takes everything (#), notifications takes only
// its actor-facing subset, exports only export.requested — and nothing
// reaches a retry tier or the DLQ on the happy path.
func TestEventRouting(t *testing.T) {
	ch := openAMQP(t)
	suffix := fmt.Sprintf(".t%d", time.Now().UnixNano())
	topo := isolatedTopology(t, ch, suffix, nil)

	// Declaring again (the publisher's reconnect path) is a no-op.
	if err := topo.Declare(ch); err != nil {
		t.Fatalf("idempotent redeclare: %v", err)
	}

	for _, key := range []string{"pr.submitted", "vendor.created", "export.requested", "grn.posted"} {
		publish(t, ch, topo.EventsExchange, key, "ev-"+key, nil)
	}

	want := map[string][]string{
		DocumentsHistoryQueue + suffix: {"export.requested", "grn.posted", "pr.submitted", "vendor.created"},
		NotificationsQueue + suffix:    {"pr.submitted"},
		ExportsQueue + suffix:          {"export.requested"},
	}
	for queue, wantKeys := range want {
		if got := drainKeys(t, ch, queue, len(wantKeys)); fmt.Sprint(got) != fmt.Sprint(wantKeys) {
			t.Errorf("queue %s got routing keys %v, want %v", queue, got, wantKeys)
		}
	}

	// Happy-path traffic never touches the failure-path destinations.
	for _, tier := range []int{1, 2, 3} {
		assertEmpty(t, ch, RetryQueueName(ExportsQueue+suffix, tier))
	}
	assertEmpty(t, ch, topo.DLQQueue)
}

// AC (failed message lands in DLQ after max attempts): a test consumer
// following the NextRetry helper fails one delivery four times — the first
// three park it on the retry ladder, each hop returning to its own main
// queue after the tier TTL; the fourth rejects without requeue and the
// message lands in the DLQ with its retry count.
func TestFailedDeliveryEndsInDLQ(t *testing.T) {
	ch := openAMQP(t)
	suffix := fmt.Sprintf(".t%d", time.Now().UnixNano())
	topo := isolatedTopology(t, ch, suffix, []time.Duration{
		150 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond,
	})
	exports := ExportsQueue + suffix
	publish(t, ch, topo.EventsExchange, "export.requested", "ev-dlq", nil)

	var deliveries int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok, err := ch.Get(exports, false)
		if err != nil {
			t.Fatalf("basic.get %s: %v", exports, err)
		}
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		deliveries++
		count := RetryCount(msg.Headers)
		dest, again := NextRetry(exports, count)
		if !again {
			// Attempt budget exhausted: reject without requeue → the queue's
			// x-dead-letter-exchange routes it to the DLX → the DLQ.
			if err := msg.Reject(false); err != nil {
				t.Fatalf("reject: %v", err)
			}
			break
		}
		// Park on the retry tier exactly as the #47 consumer will: bumped
		// x-retry-count, same body; the tier's TTL dead-letters it back.
		err = ch.PublishWithContext(t.Context(), "", dest, false, false, amqp.Publishing{
			ContentType:  msg.ContentType,
			DeliveryMode: amqp.Persistent,
			MessageId:    msg.MessageId,
			Timestamp:    msg.Timestamp,
			Headers:      amqp.Table{RetryCountHeader: count + 1},
			Body:         msg.Body,
		})
		if err != nil {
			t.Fatalf("publish to %s: %v", dest, err)
		}
		if err := msg.Ack(false); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	if deliveries != MaxRetries+1 {
		t.Fatalf("deliveries on %s = %d, want %d (first + 3 retries)", exports, deliveries, MaxRetries+1)
	}

	// The fourth failure is in the DLQ, with the retry count and the
	// dead-letter trail of the rejecting queue.
	dlq := fetchN(t, ch, topo.DLQQueue, 1)
	if dlq[0].MessageId != "ev-dlq" {
		t.Fatalf("DLQ message id = %q, want ev-dlq", dlq[0].MessageId)
	}
	if got := RetryCount(dlq[0].Headers); got != MaxRetries {
		t.Fatalf("DLQ retry count = %d, want %d", got, MaxRetries)
	}
	if q, reason := xDeath(dlq[0]); q != exports || reason != "rejected" {
		t.Fatalf("x-death = (%q, %q), want (%q, rejected)", q, reason, exports)
	}

	// Retry isolation: every hop returned to exports only — history holds
	// exactly the initial publish copy, notifications nothing, tiers empty.
	if keys := drainKeys(t, ch, DocumentsHistoryQueue+suffix, 1); fmt.Sprint(keys) != fmt.Sprint([]string{"export.requested"}) {
		t.Fatalf("history queue = %v, want only the initial publish copy", keys)
	}
	assertEmpty(t, ch, NotificationsQueue+suffix)
	for _, tier := range []int{1, 2, 3} {
		assertEmpty(t, ch, RetryQueueName(exports, tier))
	}
}

// The retry delay belongs to the broker: a message parked on a retry tier
// stays there for the tier TTL, then arrives back on its main queue with the
// x-death expired trail and the default-exchange routing key override.
func TestRetryTierWaitsOutTTL(t *testing.T) {
	ch := openAMQP(t)
	suffix := fmt.Sprintf(".t%d", time.Now().UnixNano())
	topo := isolatedTopology(t, ch, suffix, []time.Duration{
		800 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond,
	})
	exports := ExportsQueue + suffix
	retry1 := RetryQueueName(exports, 1)

	// Park directly on the tier — exactly where the consumer failure path
	// puts a first failure.
	publish(t, ch, "", retry1, "ev-ttl", amqp.Table{RetryCountHeader: int32(1)})

	// Nothing may come back before the TTL expires.
	quiet := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(quiet) {
		if msg, ok, err := ch.Get(exports, true); err != nil || ok {
			t.Fatalf("message returned before its TTL: (%q, %v)", msg.MessageId, err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	back := fetchN(t, ch, exports, 1)
	if back[0].MessageId != "ev-ttl" {
		t.Fatalf("returned message id = %q, want ev-ttl", back[0].MessageId)
	}
	if got := RetryCount(back[0].Headers); got != 1 {
		t.Fatalf("retry count after the hop = %d, want 1 (header survives the broker hop)", got)
	}
	if back[0].RoutingKey != exports {
		t.Fatalf("routing key after the hop = %q, want the main queue name %q "+
			"(x-dead-letter-routing-key through the default exchange)", back[0].RoutingKey, exports)
	}
	if q, reason := xDeath(back[0]); q != retry1 || reason != "expired" {
		t.Fatalf("x-death = (%q, %q), want (%q, expired)", q, reason, retry1)
	}
	assertEmpty(t, ch, topo.DLQQueue)
}

// xDeath reads the first dead-letter trail entry of a delivery.
func xDeath(d amqp.Delivery) (queue, reason string) {
	entries, ok := d.Headers["x-death"].([]interface{})
	if !ok || len(entries) == 0 {
		return "", ""
	}
	entry, ok := entries[0].(amqp.Table)
	if !ok {
		return "", ""
	}
	q, _ := entry["queue"].(string)
	r, _ := entry["reason"].(string)
	return q, r
}
