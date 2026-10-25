package consumer

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Retry conventions of the consumer failure path (PRD §14.5: "failures →
// retry; after max attempts → DLQ"). These constants are inline copies of
// services/worker/topology on feature/46-rabbitmq-topology-dlx-retries
// (PR #95), pinned argument-for-argument so a broker declared by either PR
// accepts the other's re-declaration without PRECONDITION_FAILED. When #46
// merges, this file is deleted and call sites switch to
// topology.RetryCount / topology.NextRetry / topology.RetryQueueName — a
// mechanical swap recorded in dev-note 047.
const (
	// RetryCountHeader carries the number of retries already attempted.
	// Absent means first delivery.
	RetryCountHeader = "x-retry-count"

	// MaxRetries is the number of retry attempts after the first delivery;
	// the (MaxRetries+1)-th failure dead-letters to the DLQ.
	MaxRetries = 3
)

// DefaultRetryTTLs is the PRD §14.5 backoff ladder: 5 s → 30 s → 5 min.
// Tests inject shorter TTLs through Topology.RetryTTLs; there is
// deliberately no production env knob — the PRD pins the ladder.
var DefaultRetryTTLs = []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}

// RetryQueueName is the retry-tier queue for a main queue (attempt is
// 1-based: 1 = the 5 s tier, 2 = 30 s, 3 = 5 min).
func RetryQueueName(queue string, attempt int) string {
	return fmt.Sprintf("%s.retry.%d", queue, attempt)
}

// NextRetry decides the failure path of one delivery from queue: the retry
// queue of tier retryCount+1 while the attempt budget lasts, ok=false when
// it is exhausted — then the caller rejects the delivery without requeue so
// the broker dead-letters it to the DLX and on to the DLQ. retryCount is
// the value of the RetryCountHeader (0 on first delivery).
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
