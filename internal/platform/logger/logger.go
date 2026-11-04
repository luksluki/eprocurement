// Package logger provides JSON structured logging with trace-id propagation.
//
// The base logger writes one JSON object per line to stdout with the service
// name and environment attached. The gateway's HTTP middleware and the gRPC
// server interceptors put the trace context into the request context (#54);
// FromContext returns a logger that includes its trace id, so every log line
// produced while handling a request carries the same trace_id.
package logger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

type ctxKey struct{}

// New returns the base JSON logger for a service.
func New(service, env, level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})).
		With("service", service, "env", env)
}

// NewTraceID returns a random 128-bit trace id as lowercase hex. It is used
// when an inbound request arrives without one.
func NewTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "" // crypto/rand failure is practically unreachable; fall back to no id
	}
	return hex.EncodeToString(b[:])
}

// WithTraceID stores the trace id in the context.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, traceID)
}

// TraceID returns the trace id for logs and outbox rows (#54): the SPAN
// context wins, so the value is exactly what Jaeger shows for the request
// — including the trace the worker's publisher/consumer re-attach by the
// outbox trace_id. The legacy context value (the pre-#54 propagation path
// via X-Trace-Id metadata, still written by the trace interceptors)
// remains the fallback for contexts that never carried a span (cron ticks,
// boot code), keeping every existing caller working.
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

type actorIPKey struct{}

// WithActorIP stores the acting client's IP in the context (#44). It rides
// the same propagation path as the trace id: gateway metadata (x-actor-ip)
// -> gRPC interceptor -> context, so outbox writers can record it next to
// the trace without another per-call parameter. Empty means unknown.
func WithActorIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, actorIPKey{}, ip)
}

// ActorIP returns the acting client's IP from the context, or "" when
// absent.
func ActorIP(ctx context.Context) string {
	ip, _ := ctx.Value(actorIPKey{}).(string)
	return ip
}

// FromContext returns a logger that includes the context trace id when present.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := TraceID(ctx); id != "" {
		return base.With("trace_id", id)
	}
	return base
}
