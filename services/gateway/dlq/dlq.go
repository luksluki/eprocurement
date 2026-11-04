// Package dlq is the gateway's small AMQP client over procurement.dlq
// (issue #48; PRD §14.5 "after max attempts → DLQ + admin visible", §16
// "DLQ monitor — admin endpoint"): List drains the DLQ (bounded) to render
// its entries for the calling tenant's admin, and Replay moves one message
// back to its original work queue with a fresh retry ladder.
//
// The gateway holding broker access is the maintainer's option-A decision
// of 2026-10-28 (issue #48 comments): a small client, no worker gRPC
// surface. The topology constants come from services/worker/topology — the
// same cross-service import inside the Go module as worker →
// procurement/outbox (documented in docs/schema/procurement.md) — and the
// DLX/DLQ declarations below are argument-identical to topology.Declare,
// so declaring on a broker that already carries the #46 topology cannot
// raise PRECONDITION_FAILED.
//
// Both operations dial a short-lived connection per call and serialize
// through one process-wide mutex: a List/Replay session holds the drained
// messages unacknowledged, and two concurrent sessions on the same broker
// would steal each other's messages (Basic.Get does not see unacked
// messages). The cross-REPLICA variant of that race remains and is
// documented in docs/ops/dlq-monitor.md.
package dlq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/luksluki/eprocurement/services/worker/topology"
)

// dialTimeout bounds one AMQP dial attempt (the worker's convention).
const dialTimeout = 10 * time.Second

// DefaultScanCap hard-bounds how many DLQ messages one List/Replay session
// drains: the tool is O(depth) per call by design (it must hold every
// message unacknowledged to inspect it), so a pathological DLQ must not
// make an admin request unbounded. Depth still reports the true size.
const DefaultScanCap = 1000

// Typed errors — the gateway maps them to HTTP (dlq_broker_unavailable →
// 503, dlq_message_not_found → 404, dlq_message_not_replayable → 409).
var (
	ErrBrokerUnavailable = errors.New("dlq_broker_unavailable")
	ErrMessageNotFound   = errors.New("dlq_message_not_found")
	ErrNotReplayable     = errors.New("dlq_message_not_replayable")
)

// Envelope is the subset of the §14.5 wire envelope List needs. Decoding is
// the worker consumer's Decode semantics exactly (services/worker/consumer/
// consumer.go: a pure json.Unmarshal — a body that unmarshals but fails
// validation still "decodes" here; one that does not unmarshal at all is a
// decode error and can never succeed on replay).
type Envelope struct {
	EventID    string    `json:"event_id"`
	TenantID   string    `json:"tenant_id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Death is the newest x-death entry the broker stamped when the message
// was dead-lettered: the queue it died on (always a main queue in this
// topology — only they carry the DLX argument), why, and when.
type Death struct {
	Queue  string
	Reason string
	DiedAt time.Time
}

// Message is one inspected DLQ entry. DecodeError marks a body that does
// not unmarshal: its business fields stay zero and its tenant cannot be
// known, so List shows it cross-tenant as metadata only (deliberate:
// visibility of poison messages outweighs the secrecy of their existence;
// the leak is existence-only and documented).
type Message struct {
	MessageID     string // AMQP MessageId = the publisher's event_id (survives a broken body)
	DecodeError   bool
	EventID       string
	TenantID      string
	Type          string
	OccurredAt    time.Time
	RetryCount    int32 // x-retry-count at death (topology.RetryCount semantics)
	OriginalQueue string
	Reason        string
	DiedAt        time.Time
}

// Owned reports whether the entry is the calling tenant's business
// message: a decoded envelope carrying that tenant_id.
func (m Message) Owned(tenantID string) bool {
	return !m.DecodeError && m.TenantID == tenantID
}

// Visible reports whether List may show the entry to the tenant: its own
// decoded entries, plus undecodable entries (cross-tenant, metadata only).
func (m Message) Visible(tenantID string) bool {
	return m.DecodeError || m.TenantID == tenantID
}

// ListResult is the admin list response: the point-in-time ready count
// (read from the declare BEFORE the drain — messages held unacknowledged
// by a drain do not count as ready, so reading after would report 0), how
// many messages the session scanned (≤ scan cap), and the visible entries.
type ListResult struct {
	Depth    int
	Scanned  int
	Messages []Message
}

// Client talks to procurement.dlq. Safe for concurrent use: one mutex
// serializes List/Replay sessions per process.
type Client struct {
	url     string
	topo    topology.Topology
	scanCap int

	mu sync.Mutex
}

// New builds a client for url. A zero topo means topology.Default; tests
// pass a Suffixed variant for broker isolation.
func New(url string, topo topology.Topology) *Client {
	if topo.DLXExchange == "" && topo.DLQQueue == "" {
		topo = topology.Default
	}
	return &Client{url: url, topo: topo, scanCap: DefaultScanCap}
}

// session is one dial → declare → drain cycle. The deferred close releases
// any still-unacknowledged deliveries back to the DLQ, so a crash mid-way
// loses nothing.
type session struct {
	conn   *amqp.Connection
	ch     *amqp.Channel
	dlq    string
	depth  int
	client *Client
}

// begin dials, bootstraps the DLX/DLQ declarations (argument-identical to
// topology.Declare), and captures the ready count BEFORE anything is
// drained from the queue.
func (c *Client) begin() (*session, error) {
	conn, err := amqp.DialConfig(c.url, amqp.Config{
		Dial: (&net.Dialer{Timeout: dialTimeout}).Dial,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: dial rabbitmq: %v", ErrBrokerUnavailable, err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: open channel: %v", ErrBrokerUnavailable, err)
	}
	q, err := declareDLQ(ch, c.topo)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &session{conn: conn, ch: ch, dlq: c.topo.DLQQueue, depth: q.Messages, client: c}, nil
}

func (s *session) close() { _ = s.conn.Close() }

// declareDLQ declares the DLX exchange, the DLQ queue, and the # binding —
// argument-identical to the same three calls inside topology.Declare, so
// this bootstrap and the worker's full declaration accept each other.
func declareDLQ(ch *amqp.Channel, topo topology.Topology) (amqp.Queue, error) {
	if err := ch.ExchangeDeclare(topo.DLXExchange, "topic",
		true, false, false, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("%w: declare exchange %s: %v",
			ErrBrokerUnavailable, topo.DLXExchange, err)
	}
	q, err := ch.QueueDeclare(topo.DLQQueue,
		true, false, false, false, nil)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("%w: declare queue %s: %v",
			ErrBrokerUnavailable, topo.DLQQueue, err)
	}
	if err := ch.QueueBind(topo.DLQQueue, "#", topo.DLXExchange, false, nil); err != nil {
		return amqp.Queue{}, fmt.Errorf("%w: bind %s to %s: %v",
			ErrBrokerUnavailable, topo.DLQQueue, topo.DLXExchange, err)
	}
	return q, nil
}

// drain Gets messages off the DLQ (autoAck=false) holding them
// unacknowledged, up to the scan cap. settle then requeues everything
// except ackIdx (the replayed copy, -1 when nothing is acked) — Nack
// errors are ignored: after a lost connection they always fail, and the
// connection close returns the deliveries to the queue anyway.
func (s *session) drain() ([]amqp.Delivery, error) {
	held := make([]amqp.Delivery, 0, 16)
	for len(held) < s.client.scanCap {
		d, ok, err := s.ch.Get(s.dlq, false)
		if err != nil {
			return held, fmt.Errorf("%w: get %s: %v", ErrBrokerUnavailable, s.dlq, err)
		}
		if !ok {
			break
		}
		held = append(held, d)
	}
	return held, nil
}

func settle(held []amqp.Delivery, ackIdx int) {
	for i, d := range held {
		if i == ackIdx {
			_ = d.Ack(false)
			continue
		}
		_ = d.Nack(false, true) // requeue: the DLQ keeps its message
	}
}

// List renders the DLQ for tenantID: its decoded entries plus undecodable
// entries (cross-tenant, metadata only), at most limit of them. limit <= 0
// means unlimited (the server clamps before calling).
func (c *Client) List(ctx context.Context, tenantID string, limit int) (ListResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	s, err := c.begin()
	if err != nil {
		return ListResult{}, err
	}
	defer s.close()

	held, err := s.drain()
	out := ListResult{Depth: s.depth, Scanned: len(held), Messages: []Message{}}
	settle(held, -1)
	if err != nil {
		return ListResult{}, err
	}
	for _, d := range held {
		m := parse(d)
		if !m.Visible(tenantID) {
			continue
		}
		if limit > 0 && len(out.Messages) >= limit {
			break
		}
		out.Messages = append(out.Messages, m)
	}
	return out, nil
}

// Replay moves the DLQ message whose AMQP MessageId is messageID (the
// publisher's event_id — a property, so it survives a broken body) back to
// the work queue it died on (x-death), through the default exchange with
// x-retry-count reset to 0 (maintainer decision 2026-10-28: a replayed
// message gets the full fresh 3-attempt ladder) and the stale x-death
// dropped. Body, MessageId, and ContentType are preserved verbatim.
//
// The DLQ copy is acked only after the publish succeeded; a failed publish
// requeues everything untouched (the consumer's publishRetry pattern,
// consumer.go). Consumers dedupe by event_id, so a crash between publish
// and ack at worst replays a duplicate (at-least-once, PRD §14.8).
func (c *Client) Replay(ctx context.Context, tenantID, messageID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	s, err := c.begin()
	if err != nil {
		return err
	}
	defer s.close()

	held, err := s.drain()
	if err != nil {
		settle(held, -1)
		return err
	}

	targets := map[string]bool{}
	for _, q := range c.topo.Queues {
		targets[q.Name] = true
	}

	idx := -1
	var target string
	for i, d := range held {
		if d.MessageId != messageID {
			continue
		}
		m := parse(d)
		if err := replayDecision(m, tenantID, targets); err != nil {
			settle(held, -1)
			return err
		}
		idx, target = i, m.OriginalQueue
		break
	}
	if idx == -1 {
		settle(held, -1)
		return fmt.Errorf("%w: no DLQ message %q in the first %d scanned",
			ErrMessageNotFound, messageID, len(held))
	}

	d := held[idx]
	if err := s.publishReplay(ctx, d, target); err != nil {
		settle(held, -1)
		return err
	}
	settle(held, idx) // ack the replayed copy only now; requeue the rest
	return nil
}

// replayDecision is the pure replay gate, factored for the unit table:
// an undecodable message is never replayable (409 — replaying it would
// only bounce it back to the DLQ, consumer.go's permanent-failure path;
// removal goes through broker tooling per the runbook); a decoded message
// of another tenant (or with no tenant) is "not found" (404 — cross-tenant
// opacity: the caller learns nothing); a decoded owned message without a
// known target queue is not replayable either (409).
func replayDecision(m Message, tenantID string, targets map[string]bool) error {
	if m.DecodeError {
		return fmt.Errorf("%w: message body does not decode", ErrNotReplayable)
	}
	if !m.Owned(tenantID) {
		return fmt.Errorf("%w: message %q is not owned by this tenant", ErrMessageNotFound, m.MessageID)
	}
	if !targets[m.OriginalQueue] {
		return fmt.Errorf("%w: no known target queue %q", ErrNotReplayable, m.OriginalQueue)
	}
	return nil
}

// publishReplay republishes the DLQ copy to the work queue via the default
// exchange (routing key = queue name — the same route the retry tiers
// use), with x-retry-count reset to 0 and x-death dropped.
func (s *session) publishReplay(ctx context.Context, d amqp.Delivery, target string) error {
	headers := amqp.Table{}
	for k, v := range d.Headers {
		if k == "x-death" {
			continue // stale death trail: the replay starts a fresh ladder
		}
		headers[k] = v
	}
	headers[topology.RetryCountHeader] = int32(0)
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.ch.PublishWithContext(pctx, "", target, false, false, amqp.Publishing{
		Headers:      headers,
		ContentType:  d.ContentType,
		Type:         d.Type,
		MessageId:    d.MessageId,
		Timestamp:    d.Timestamp,
		DeliveryMode: amqp.Persistent,
		Body:         d.Body,
	}); err != nil {
		return fmt.Errorf("%w: replay publish to %s: %v", ErrBrokerUnavailable, target, err)
	}
	return nil
}

// parse inspects one DLQ delivery into a Message. It never fails: anything
// undecodable or missing becomes the metadata the entry still has.
func parse(d amqp.Delivery) Message {
	m := Message{
		MessageID:  d.MessageId,
		RetryCount: topology.RetryCount(d.Headers),
	}
	var env Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		m.DecodeError = true
	} else {
		m.EventID, m.TenantID, m.Type, m.OccurredAt = env.EventID, env.TenantID, env.Type, env.OccurredAt
	}
	if death, ok := newestDeath(d.Headers); ok {
		m.OriginalQueue, m.Reason, m.DiedAt = death.Queue, death.Reason, death.DiedAt
	}
	return m
}

// newestDeath reads the newest x-death entry (the broker prepends, so it
// is element 0). Tolerant of a missing or oddly-typed header: only main
// queues dead-letter to the DLX in this topology, and their entry names
// the queue a replay must return to.
func newestDeath(h amqp.Table) (Death, bool) {
	entries, ok := h["x-death"].([]any)
	if !ok || len(entries) == 0 {
		return Death{}, false
	}
	first, ok := entries[0].(amqp.Table)
	if !ok {
		return Death{}, false
	}
	death := Death{Queue: stringOf(first["queue"]), Reason: stringOf(first["reason"])}
	if t, ok := first["time"].(time.Time); ok {
		death.DiedAt = t
	}
	return death, true
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
