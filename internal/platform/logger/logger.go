// Package logger provides JSON structured logging with trace-id propagation.
//
// The base logger writes one JSON object per line to stdout with the service
// name and environment attached. gRPC interceptors (see services/identity/server)
// put the trace id into the request context; FromContext returns a logger that
// includes it, so every log line produced while handling a request carries the
// same trace_id.
package logger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
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

// TraceID returns the trace id from the context, or "" when absent.
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// FromContext returns a logger that includes the context trace id when present.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := TraceID(ctx); id != "" {
		return base.With("trace_id", id)
	}
	return base
}
