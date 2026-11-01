package export

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// DefaultAgeInterval is the export_age_seconds refresh cadence (#55): the
// gauge is recomputed often enough that the 26 h staleness alert (FR-10,
// deploy/compose/prometheus/alerts.yml) reflects reality without waiting a
// full scheduler slot.
const DefaultAgeInterval = 60 * time.Second

// AgeSink receives the per-tenant export-age samples. It is satisfied by
// *internal/platform/metrics.Collectors (SetExportAge/DeleteExportAge), so
// this package stays free of the Prometheus dependency and unit-testable
// with a plain fake.
type AgeSink interface {
	// SetExportAge publishes the tenant's age-of-last-success in seconds.
	SetExportAge(tenantID string, age time.Duration)
	// DeleteExportAge removes the tenant's series (an honest gap: the age
	// is unknown, not zero).
	DeleteExportAge(tenantID string)
}

// AgeMonitor refreshes the export_age_seconds{tenant} gauge (#55, FR-10,
// dev-note 049 "gauge + alert"): every Interval it enumerates the tenants
// (the register's SELECT DISTINCT tenant_id) and reads each tenant's
// job:export:last_success heartbeat in Redis. A valid heartbeat sets the
// age; an absent, malformed, or unreadable heartbeat DELETES the series —
// a data gap, never a faked 0, so a missing signal can neither fire the
// 26 h alert for a healthy tenant nor mask a stale one behind a fake
// fresh reading. A tenant that leaves the enumeration (no purchase orders
// anymore) also loses its series, so no ghost staleness survives.
type AgeMonitor struct {
	Tenants  TenantLister
	KV       KV
	Sink     AgeSink
	Interval time.Duration // zero means DefaultAgeInterval
	Logger   *slog.Logger

	// now pins the age clock in tests; exposed tracks the tenants whose
	// series exist so tenants leaving the enumeration are cleaned up.
	now     func() time.Time
	exposed map[string]bool
}

// NewAgeMonitor binds the monitor; interval <= 0 means DefaultAgeInterval.
func NewAgeMonitor(tenants TenantLister, kv KV, sink AgeSink, interval time.Duration, log *slog.Logger) *AgeMonitor {
	if interval <= 0 {
		interval = DefaultAgeInterval
	}
	return &AgeMonitor{
		Tenants:  tenants,
		KV:       kv,
		Sink:     sink,
		Interval: interval,
		Logger:   log,
		now:      time.Now,
		exposed:  map[string]bool{},
	}
}

// Run ticks immediately, then every Interval, until ctx is cancelled —
// then returns nil. Tenant-enumeration failures are a warn, never fatal
// (the publisher/dlqmonitor convention): the gauge set keeps its last
// real values until the next successful tick.
func (a *AgeMonitor) Run(ctx context.Context) error {
	interval := a.Interval
	if interval <= 0 {
		interval = DefaultAgeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	a.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

// tick recomputes every enumerated tenant's age.
func (a *AgeMonitor) tick(ctx context.Context) {
	tenants, err := a.Tenants.Tenants(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.logger().Warn("export age refresh skipped: cannot enumerate tenants", "error", err)
		}
		return
	}
	next := make(map[string]bool, len(tenants))
	for _, tenantID := range tenants {
		value, ok, err := a.KV.Get(ctx, HeartbeatKey(tenantID))
		switch {
		case err != nil:
			if ctx.Err() == nil { // mid-tick shutdown is not an unreadable heartbeat
				a.logger().Warn("export age heartbeat unreadable; series removed",
					"tenant_id", tenantID, "error", err)
			}
			a.Sink.DeleteExportAge(tenantID)
		case !ok:
			// Never exported (or Redis flushed): unknown age, not zero.
			a.logger().Info("export age heartbeat absent; series removed", "tenant_id", tenantID)
			a.Sink.DeleteExportAge(tenantID)
		default:
			last, err := ParseHeartbeat(value)
			if err != nil {
				a.logger().Warn("export age heartbeat malformed; series removed", "tenant_id", tenantID)
				a.Sink.DeleteExportAge(tenantID)
				continue
			}
			age := a.now().Sub(last)
			if age < 0 {
				age = 0 // a clock skew cannot publish negative age
			}
			a.Sink.SetExportAge(tenantID, age)
			next[tenantID] = true
		}
	}
	// Tenants that left the enumeration must not keep a frozen series.
	for tenantID := range a.exposed {
		if !next[tenantID] {
			a.Sink.DeleteExportAge(tenantID)
			delete(a.exposed, tenantID)
		}
	}
	a.exposed = next
}

func (a *AgeMonitor) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
