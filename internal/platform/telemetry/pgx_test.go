package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// AC #5 (DB spans): every SQL statement gets a span carrying the caller's
// trace context, and the span ends — error-marked — on failure.
func TestPGXTracerStartsSpanPerQuery(t *testing.T) {
	parent, span := testTracer.Start(context.Background(), "grpc /pkg.Svc/M")
	defer span.End()
	want := spanContextOf(parent).TraceID().String()

	tracer := PGXTracer{}
	ctx := tracer.TraceQueryStart(parent, nil, pgx.TraceQueryStartData{SQL: "SELECT id FROM procurement.vendors WHERE tenant_id = $1"})
	if got := spanContextOf(ctx).TraceID().String(); got != want {
		t.Fatalf("pgx span trace id = %q, want the caller's %q (DB spans must join the request trace)", got, want)
	}
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{}) // no error, no panic
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("boom")})
}

// PGXPoolConfig attaches the tracer to the pool config (the wiring the
// three runtime pools use) and still rejects invalid URLs.
func TestPGXPoolConfig(t *testing.T) {
	cfg, err := PGXPoolConfig("postgres://u:p@localhost:5432/db?sslmode=disable")
	if err != nil {
		t.Fatalf("PGXPoolConfig: %v", err)
	}
	if cfg.ConnConfig.Tracer == nil {
		t.Fatal("pool config carries no pgx tracer")
	}

	// Failure path: a URL that cannot parse returns the error.
	if _, err := PGXPoolConfig("://not-a-url"); err == nil {
		t.Fatal("PGXPoolConfig accepted an invalid URL")
	}
}
