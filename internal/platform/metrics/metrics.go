// Package metrics provides the process-local Prometheus registry every Go
// process exposes on its METRICS_PORT (#55, PRD §18): the request-latency
// histograms (HTTP at the gateway, gRPC on the internal services — wired
// through the grpcserver chain), the worker's operational gauges (outbox
// lag, DLQ depth, per-tenant export age), the DLQ dead-letter counter
// (#115), and the small HTTP server that publishes them at /metrics. Each
// process builds one Collectors value in main, hands it to its server
// scaffolding, and runs Serve alongside the main listener; the ports are
// container-internal; only Prometheus (compose profile observability)
// scrapes them.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// latencyBuckets resolve the PRD §19 p95 targets (300 ms list endpoints)
// with sub-100 ms resolution and still catch tail outliers up to 10 s.
var latencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5, 10,
}

// DLQ dead-letter reasons — the closed set the worker's consumers report
// (the "reason" field of the dead-letter log lines in services/worker/
// consumer/consumer.go and the reason label of dlq_deliveries_total).
// Keep the two in sync: the consumer's OnDeadLetter hook feeds
// IncDLQDeliveries, and New() pre-creates exactly these series.
const (
	DLQReasonDecodeFailed    = "decode_failed"
	DLQReasonInvalidEnvelope = "invalid_envelope"
	DLQReasonInvalidEvent    = "invalid_event"
	DLQReasonMaxRetries      = "max_retries"
)

// dlqDeliveryReasons is the reason set New() pre-initializes at 0.
var dlqDeliveryReasons = []string{
	DLQReasonDecodeFailed, DLQReasonInvalidEnvelope,
	DLQReasonInvalidEvent, DLQReasonMaxRetries,
}

// Collectors owns the process's private registry and the #55 metric
// families. A fresh Collectors value registers nothing on the DefaultGatherer,
// so tests and multiple constructions never collide.
type Collectors struct {
	registry *prometheus.Registry

	// httpDuration — gateway REST latency (PRD §18 "request latency").
	// The route label is the route TEMPLATE (/api/v1/vendors/:id), never
	// the concrete path, so cardinality stays bounded.
	httpDuration *prometheus.HistogramVec
	// grpcDuration — internal gRPC latency, recorded by the interceptor
	// this package provides and grpcserver chains.
	grpcDuration *prometheus.HistogramVec
	// outboxLag — seconds since the oldest unpublished procurement.outbox
	// row (PRD §18 "outbox lag"); 0 means fully drained.
	outboxLag prometheus.Gauge
	// dlqDepth — ready messages in procurement.dlq (PRD §18 "DLQ depth").
	dlqDepth prometheus.Gauge
	// exportAge — seconds since each tenant's last successful nightly
	// export (FR-10, dev-note 049). A tenant without a readable heartbeat
	// carries NO series (DeleteExportAge) — a gap, never a faked 0, so a
	// missing signal cannot fire the 26 h staleness alert.
	exportAge *prometheus.GaugeVec
	// dlqDeliveries — dead-letter deliveries to procurement.dlq by reason
	// (#115, PRD §18). The reason set is closed (see the DLQReason*
	// constants) and every child is pre-created at 0 in New().
	dlqDeliveries *prometheus.CounterVec
}

// New builds the collectors and registers them on a private registry along
// with the standard Go runtime/process collectors.
func New() *Collectors {
	c := &Collectors{registry: prometheus.NewRegistry()}
	c.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	c.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Gateway REST request latency in seconds (#55, PRD §18).",
		Buckets: latencyBuckets,
	}, []string{"method", "route", "status"})
	c.grpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "grpc_request_duration_seconds",
		Help:    "Internal gRPC unary request latency in seconds (#55, PRD §18).",
		Buckets: latencyBuckets,
	}, []string{"method", "code"})
	c.outboxLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_lag_seconds",
		Help: "Age of the oldest unpublished procurement.outbox row in seconds; 0 when drained (#55, PRD §18).",
	})
	c.dlqDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dlq_depth_messages",
		Help: "Ready messages in the procurement.dlq dead-letter queue (#55, PRD §18).",
	})
	c.exportAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "export_age_seconds",
		Help: "Seconds since the tenant's last successful nightly export (FR-10); no series = no readable heartbeat (#55).",
	}, []string{"tenant"})
	c.dlqDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dlq_deliveries_total",
		Help: "Dead-letter deliveries to procurement.dlq by reason (#115, PRD §18); reason set is closed and pre-initialized at 0 (see DLQReason* constants).",
	}, []string{"reason"})
	// Pre-initialize every reason series at 0 (#115). /metrics is served
	// with plain promhttp.HandlerOpts{} — no OpenMetrics created timestamps
	// (client_golang's EnableOpenMetrics / EnableOpenMetricsTextCreatedSamples
	// are opt-in, default false) — so a counter series that first appears
	// mid-window already at its final value makes increase() compute
	// last − first = 0: a single dead-letter per reason would never fire
	// DLQDeadLetterRate. Zero-valued children from process start make the
	// 0→1 step between two scrapes observable, and also cover a worker
	// that dead-letters before its first scrape. Side effect: every
	// process exports the four zero series; the alert scopes to
	// {job="worker"}, and cardinality is bounded by the closed reason set.
	for _, reason := range dlqDeliveryReasons {
		c.dlqDeliveries.WithLabelValues(reason).Add(0)
	}
	c.registry.MustRegister(c.httpDuration, c.grpcDuration, c.outboxLag, c.dlqDepth, c.exportAge, c.dlqDeliveries)
	return c
}

// Registry exposes the private registry's gatherer — tests and tooling read
// the exposition without a live /metrics server.
func (c *Collectors) Registry() prometheus.Gatherer { return c.registry }

// ObserveHTTP records one gateway request. status is the final response
// status; route is the matched route template.
func (c *Collectors) ObserveHTTP(method, route string, status int, d time.Duration) {
	c.httpDuration.WithLabelValues(method, route, strconv.Itoa(status)).Observe(d.Seconds())
}

// SetOutboxLag publishes the outbox lag gauge. Callers keep the last value
// on poll failure (stale but real beats a faked 0).
func (c *Collectors) SetOutboxLag(d time.Duration) {
	c.outboxLag.Set(d.Seconds())
}

// SetDLQDepth publishes the DLQ depth gauge.
func (c *Collectors) SetDLQDepth(n int) {
	c.dlqDepth.Set(float64(n))
}

// SetExportAge publishes one tenant's export-age series (recreating it if
// it was previously deleted).
func (c *Collectors) SetExportAge(tenantID string, age time.Duration) {
	c.exportAge.WithLabelValues(tenantID).Set(age.Seconds())
}

// DeleteExportAge removes one tenant's series — an honest gap: the tenant
// has no readable heartbeat, so its age is unknown, not zero, and it must
// not fire (nor mask) the 26 h staleness alert. Deleting an absent series
// is a no-op.
func (c *Collectors) DeleteExportAge(tenantID string) {
	c.exportAge.DeleteLabelValues(tenantID)
}

// IncDLQDeliveries counts one dead-lettered delivery. The worker wires it
// as the consumer runners' OnDeadLetter hook (#115); reason must be one of
// the DLQReason* constants (the set New() pre-initialized, matching the
// consumer's dead-letter log lines).
func (c *Collectors) IncDLQDeliveries(reason string) {
	c.dlqDeliveries.WithLabelValues(reason).Inc()
}
