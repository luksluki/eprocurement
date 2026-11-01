// Package metrics provides the process-local Prometheus registry every Go
// process exposes on its METRICS_PORT (#55, PRD §18): the request-latency
// histograms (HTTP at the gateway, gRPC on the internal services — wired
// through the grpcserver chain), the worker's operational gauges (outbox
// lag, DLQ depth, per-tenant export age), and the small HTTP server that
// publishes them at /metrics. Each process builds one Collectors value in
// main, hands it to its server scaffolding, and runs Serve alongside the
// main listener; the ports are container-internal; only Prometheus
// (compose profile observability) scrapes them.
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
	c.registry.MustRegister(c.httpDuration, c.grpcDuration, c.outboxLag, c.dlqDepth, c.exportAge)
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
