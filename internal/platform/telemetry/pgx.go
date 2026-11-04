package telemetry

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// dbTracer names the instrumentation scope of the pgx query tracer.
var dbTracer = otel.Tracer("github.com/luksluki/eprocurement/internal/platform/telemetry/pgx")

// PGXTracer implements pgx.QueryTracer: one span per SQL statement, named
// by the statement's verb (SELECT/INSERT/...) so Jaeger groups read/write
// work at a glance. Attributes carry db.system and the SQL TEXT ONLY —
// pgx binds arguments separately ($1, $2, ...) and they are never put on
// the span, keeping parameters and any values they contain out of the
// telemetry pipeline (AGENTS.md: no secrets in logs/traces).
//
// Attached to the three RUNTIME pools (procurement, identity, worker) via
// PGXPoolConfig. The boot-time migration pools are deliberately excluded:
// they run before the service serves traffic, once per deploy, on the
// simple protocol — see the #54 dev note for the rationale.
type PGXTracer struct{}

var _ pgx.QueryTracer = PGXTracer{}

// TraceQueryStart starts the span and returns a context carrying it.
func (PGXTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx, span := dbTracer.Start(ctx, "pgx "+sqlVerb(data.SQL),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", data.SQL),
		))
	return trace.ContextWithSpan(ctx, span)
}

// TraceQueryEnd ends the span, marking errors.
func (PGXTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if data.Err != nil {
		span.SetStatus(codes.Error, data.Err.Error())
		span.RecordError(data.Err)
	}
	span.End()
}

// sqlVerb returns the first SQL keyword uppercased ("SELECT"); statements
// that do not start with a word (edge cases) fall back to "query".
func sqlVerb(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return "QUERY"
	}
	return strings.ToUpper(fields[0])
}

// PGXPoolConfig parses a database URL into a pool config with the query
// tracer attached — the one-line wiring the three runtime pools use.
func PGXPoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = PGXTracer{}
	return cfg, nil
}
