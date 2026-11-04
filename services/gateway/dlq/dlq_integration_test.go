package dlq

// Integration tests against a real RabbitMQ (TEST_RABBITMQ_URL; CI has no
// broker service and is not changed — same skip pattern as the publisher,
// topology, and consumer suites). Every test uses its own Suffixed
// topology and seeds the DLQ through the REAL dead-letter path: publish to
// the events exchange, Basic.Get the delivery off a main queue with
// autoAck=false, and Nack without requeue — the queue's DLX argument routes
// it to the DLQ with a genuine x-death trail, which is what List parses.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/services/worker/topology"
)

const (
	itPoll     = 50 * time.Millisecond
	itDeadline = 15 * time.Second
)

type itEnv struct {
	amqpURL  string
	topo     topology.Topology
	testConn *amqp.Connection
	hist     string // suffixed main queue names
	notif    string
}

func newItEnv(t *testing.T) *itEnv {
	t.Helper()
	amqpURL := os.Getenv("TEST_RABBITMQ_URL")
	if amqpURL == "" {
		t.Skip("TEST_RABBITMQ_URL not set; skipping integration test")
	}
	topo := topology.Default.Suffixed(fmt.Sprintf("-dlqit-%d", time.Now().UnixNano()))
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
	return &itEnv{
		amqpURL: amqpURL, topo: topo, testConn: conn,
		hist: topo.Queues[0].Name, notif: topo.Queues[1].Name,
	}
}

func (e *itEnv) channel() *amqp.Channel {
	ch, err := e.testConn.Channel()
	if err != nil {
		panic(err)
	}
	return ch
}

// deadLetter routes one message into the DLQ through the real failure
// path: publish to the events exchange, take it off queue with autoAck=
// false, and reject it — the main queue's x-dead-letter-exchange does the
// rest. retryCount, when >= 0, travels in the x-retry-count header. The
// routing key can match several queues (e.g. pr.submitted binds history
// AND notifications), so the helper scans the queue for the exact message
// id instead of assuming the FIFO head is the fresh publish.
func (e *itEnv) deadLetter(t *testing.T, queue, routingKey string, body []byte, messageID string, retryCount int32) {
	t.Helper()
	ch := e.channel()
	defer ch.Close()
	headers := amqp.Table{}
	if retryCount >= 0 {
		headers[topology.RetryCountHeader] = retryCount
	}
	pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.PublishWithContext(pctx, e.topo.EventsExchange, routingKey, false, false,
		amqp.Publishing{
			ContentType: "application/json", MessageId: messageID,
			Headers: headers, DeliveryMode: amqp.Persistent, Body: body,
		}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(itDeadline)
	held := []amqp.Delivery{}
	found := false
	for !found {
		if time.Now().After(deadline) {
			t.Fatalf("message %s never reached %s", messageID, queue)
		}
		d, ok, err := ch.Get(queue, false)
		if err != nil {
			t.Fatalf("get from %s: %v", queue, err)
		}
		if !ok {
			time.Sleep(itPoll)
			continue
		}
		if d.MessageId == messageID {
			_ = d.Nack(false, false) // rejected → DLX → DLQ
			found = true
			break
		}
		held = append(held, d) // an older delivery; put it back afterwards
	}
	for _, d := range held {
		_ = d.Nack(false, true)
	}
}

func (e *itEnv) depth(t *testing.T, queue string) int {
	t.Helper()
	ch := e.channel()
	defer ch.Close()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatalf("passive declare %s: %v", queue, err)
	}
	return q.Messages
}

func waitUntil(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(itDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(itPoll)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// AC (#48): a dead-lettered message is visible in the admin list — decoded
// entries of the calling tenant with their event metadata and the x-death
// trail, undecodable entries cross-tenant as metadata only, another
// tenant's decoded entries filtered out. Depth is the ready count read
// BEFORE the drain (the same call's drain holds every message unacked, yet
// depth reports the planted size), scanned reports the drain size, the
// scan cap bounds it, and limit bounds the response.
func TestListSeedsAndFilters(t *testing.T) {
	e := newItEnv(t)
	ctx := t.Context()
	client := New(e.amqpURL, e.topo)

	e.deadLetter(t, e.hist, "pr.submitted", envelopeBody("evt-a1", "tenant-a", "pr.submitted"), "evt-a1", 3)
	e.deadLetter(t, e.hist, "vendor.created", envelopeBody("evt-a2", "tenant-a", "vendor.created"), "evt-a2", -1)
	e.deadLetter(t, e.notif, "pr.approved", envelopeBody("evt-b1", "tenant-b", "pr.approved"), "evt-b1", 1)
	e.deadLetter(t, e.hist, "pr.submitted", []byte("{not json"), "evt-x", -1)
	waitUntil(t, "dlq holds 4 messages", func() bool { return e.depth(t, e.topo.DLQQueue) == 4 })

	res, err := client.List(ctx, "tenant-a", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Depth != 4 || res.Scanned != 4 {
		t.Fatalf("depth/scanned = %d/%d, want 4/4 (depth is read before the drain holds them unacked)",
			res.Depth, res.Scanned)
	}
	if len(res.Messages) != 3 {
		t.Fatalf("tenant-a sees %d messages, want 3 (own evt-a1, evt-a2 + the undecodable evt-x; evt-b1 filtered): %+v",
			len(res.Messages), res.Messages)
	}
	byID := map[string]Message{}
	for _, m := range res.Messages {
		byID[m.MessageID] = m
	}
	a1 := byID["evt-a1"]
	if a1.DecodeError || a1.EventID != "evt-a1" || a1.TenantID != "tenant-a" ||
		a1.Type != "pr.submitted" || a1.RetryCount != 3 ||
		a1.OriginalQueue != e.hist || a1.Reason != "rejected" || a1.DiedAt.IsZero() {
		t.Fatalf("evt-a1 entry = %+v", a1)
	}
	x := byID["evt-x"]
	if !x.DecodeError || x.TenantID != "" || x.EventID != "" ||
		x.OriginalQueue != e.hist || x.Reason != "rejected" {
		t.Fatalf("evt-x entry = %+v, want metadata-only decode_error", x)
	}

	// The other tenant sees its own decoded entry — plus the undecodable
	// entry, which is cross-tenant by design (metadata only).
	resB, err := client.List(ctx, "tenant-b", 0)
	if err != nil {
		t.Fatalf("list tenant-b: %v", err)
	}
	if len(resB.Messages) != 2 {
		t.Fatalf("tenant-b sees %d messages, want 2 (evt-b1 + the cross-tenant evt-x): %+v",
			len(resB.Messages), resB.Messages)
	}
	b1, x1 := resB.Messages[0], resB.Messages[1]
	if b1.MessageID != "evt-b1" || b1.OriginalQueue != e.notif || b1.DecodeError {
		t.Fatalf("tenant-b entry = %+v, want decoded evt-b1 from %s", b1, e.notif)
	}
	if x1.MessageID != "evt-x" || !x1.DecodeError || x1.TenantID != "" {
		t.Fatalf("tenant-b metadata entry = %+v, want decode_error evt-x", x1)
	}

	// limit bounds the response, not the scan.
	limited, err := client.List(ctx, "tenant-a", 1)
	if err != nil {
		t.Fatalf("limited list: %v", err)
	}
	if len(limited.Messages) != 1 || limited.Scanned != 4 {
		t.Fatalf("limited list = %d messages / %d scanned, want 1/4", len(limited.Messages), limited.Scanned)
	}

	// The scan cap bounds the drain; depth still reports the true size.
	capped := New(e.amqpURL, e.topo)
	capped.scanCap = 2
	cappedRes, err := capped.List(ctx, "tenant-a", 0)
	if err != nil {
		t.Fatalf("capped list: %v", err)
	}
	if cappedRes.Scanned != 2 || cappedRes.Depth != 4 || len(cappedRes.Messages) > 2 {
		t.Fatalf("capped list = %d scanned / depth %d / %d messages, want 2/4/≤2",
			cappedRes.Scanned, cappedRes.Depth, len(cappedRes.Messages))
	}
}

// AC (#48): replay moves the message back to its original work queue with
// the retry ladder reset — while another tenant's message answers 404
// without being touched, and an undecodable message answers 409 and stays.
func TestReplayMovesResetsAndRefuses(t *testing.T) {
	e := newItEnv(t)
	ctx := t.Context()
	client := New(e.amqpURL, e.topo)

	e.deadLetter(t, e.hist, "pr.submitted", envelopeBody("evt-a1", "tenant-a", "pr.submitted"), "evt-a1", 3)
	e.deadLetter(t, e.notif, "pr.approved", envelopeBody("evt-b1", "tenant-b", "pr.approved"), "evt-b1", 1)
	e.deadLetter(t, e.hist, "pr.submitted", []byte("{not json"), "evt-x", -1)
	waitUntil(t, "dlq holds 3 messages", func() bool { return e.depth(t, e.topo.DLQQueue) == 3 })

	// Cross-tenant replay: 404 and the message is untouched.
	err := client.Replay(ctx, "tenant-a", "evt-b1")
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("cross-tenant replay err = %v, want ErrMessageNotFound", err)
	}
	if got := e.depth(t, e.topo.DLQQueue); got != 3 {
		t.Fatalf("dlq depth after refused replay = %d, want 3", got)
	}

	// Undecodable replay: 409, untouched (removal is broker tooling).
	err = client.Replay(ctx, "tenant-a", "evt-x")
	if !errors.Is(err, ErrNotReplayable) {
		t.Fatalf("undecodable replay err = %v, want ErrNotReplayable", err)
	}
	if got := e.depth(t, e.topo.DLQQueue); got != 3 {
		t.Fatalf("dlq depth after unreplayable = %d, want 3", got)
	}

	// Unknown message id: 404.
	if err := client.Replay(ctx, "tenant-a", "evt-nope"); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("unknown replay err = %v, want ErrMessageNotFound", err)
	}

	// Owned replay: the message leaves the DLQ…
	if err := client.Replay(ctx, "tenant-a", "evt-a1"); err != nil {
		t.Fatalf("owned replay: %v", err)
	}
	waitUntil(t, "dlq back to 2", func() bool { return e.depth(t, e.topo.DLQQueue) == 2 })

	// …and arrives on its original work queue with the ladder reset:
	// x-retry-count explicitly 0, no x-death, body/MessageId/ContentType
	// preserved. (The suffixed history queue may hold unrelated copies of
	// notifications-bound events; scan for the replayed id.)
	ch := e.channel()
	defer ch.Close()
	waitUntil(t, "replay lands on "+e.hist, func() bool { return e.depth(t, e.hist) >= 1 })
	var found *amqp.Delivery
	held := []amqp.Delivery{}
	for range 10 {
		d, ok, err := ch.Get(e.hist, false)
		if err != nil || !ok {
			break
		}
		held = append(held, d)
		if d.MessageId == "evt-a1" {
			found = &held[len(held)-1]
			break
		}
	}
	if found == nil {
		t.Fatalf("replayed evt-a1 not found on %s", e.hist)
	}
	for i := range held {
		if found == &held[i] {
			_ = held[i].Ack(false)
		} else {
			_ = held[i].Nack(false, true)
		}
	}
	if v, ok := found.Headers[topology.RetryCountHeader]; !ok || topology.RetryCount(found.Headers) != 0 || v == nil {
		t.Fatalf("replayed x-retry-count = %#v, want an explicit 0", found.Headers[topology.RetryCountHeader])
	}
	if _, dead := found.Headers["x-death"]; dead {
		t.Fatalf("replayed message still carries x-death: %#v", found.Headers["x-death"])
	}
	if string(found.Body) != string(envelopeBody("evt-a1", "tenant-a", "pr.submitted")) ||
		found.ContentType != "application/json" {
		t.Fatalf("replayed body/content-type not preserved: %q / %q", found.Body, found.ContentType)
	}
}

// A broker that is not reachable is a typed 503 for both operations.
func TestBrokerUnavailable(t *testing.T) {
	client := New("amqp://127.0.0.1:1/", topology.Default)
	if _, err := client.List(t.Context(), "tenant-a", 0); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("list err = %v, want ErrBrokerUnavailable", err)
	}
	if err := client.Replay(t.Context(), "tenant-a", "evt-1"); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("replay err = %v, want ErrBrokerUnavailable", err)
	}
}
