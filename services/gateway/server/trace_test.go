package server

// AC #54 (edge → gRPC hand-off): the OTel HTTP middleware stores the
// request span with c.SetUserContext, forwardContext builds the outgoing
// gRPC context from c.UserContext(), and the OTel client interceptor
// injects traceparent into the outgoing metadata from exactly that
// context. Pinned end-to-end here WITHOUT a live gRPC hop: the fake
// vendor client captures the context forwardContext produced, then the
// real client interceptor runs over it and the injected traceparent must
// carry the SAME trace id as the inbound request.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/jwt"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/internal/platform/telemetry"
	"github.com/luksluki/eprocurement/services/identity/auth"
	"go.opentelemetry.io/otel/trace"
)

func TestForwardContextCarriesRequestSpan(t *testing.T) {
	const inboundTraceID = "0af7651916cd43dd8448eb211c80319c"

	// The process's SDK (production does this in main; without it the otel
	// global is the no-op tracer and nothing records).
	tp, err := telemetry.Setup(context.Background(), "gateway-test", "local", "")
	if err != nil {
		t.Fatalf("setup telemetry: %v", err)
	}
	defer telemetry.Shutdown(context.Background(), tp, time.Second)

	app, verifier, priv, kid := newTestApp(t)
	vendors := &fakeVendorClient{listRes: &procurementv1.ListVendorsResponse{}}
	registerVendorRoutes(app, verifier, vendors, nil)

	now := time.Now()
	tokenStr, err := jwt.Sign(priv, kid, auth.AccessClaims{
		Sub: "user-54", TenantID: "tenant-54", Roles: []string{rbac.RoleViewer},
		IAT: now.Unix(), EXP: now.Add(time.Minute).Unix(),
	}, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	resp := doReq(t, app, func() http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+tokenStr)
		h.Set("traceparent", "00-"+inboundTraceID+"-00f067aa0ba902b7-01")
		return h
	}(), http.MethodGet, "/api/v1/vendors", "")

	if vendors.lastCtx == nil {
		t.Fatal("vendors not called")
	}

	// 1. The span context the middleware installed survived forwardContext.
	sc := trace.SpanContextFromContext(vendors.lastCtx)
	if !sc.IsValid() {
		t.Fatal("forwardContext context carries no span; c.SetUserContext wiring broken")
	}
	if got := sc.TraceID().String(); got != inboundTraceID {
		t.Fatalf("outgoing span trace id = %q, want the inbound %q", got, inboundTraceID)
	}

	// 2. The legacy x-trace-id metadata equals the span trace id (derived,
	// never independent).
	md, _ := metadata.FromOutgoingContext(vendors.lastCtx)
	if got := md.Get("x-trace-id"); len(got) != 1 || got[0] != inboundTraceID {
		t.Fatalf("x-trace-id metadata = %v, want [%s]", got, inboundTraceID)
	}

	// 3. The real client interceptor, run over that exact context, injects
	// traceparent with the same trace id — the wire proof of the hand-off.
	var intercepted context.Context
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		intercepted = ctx
		return nil
	}
	if err := telemetry.GRPCUnaryClientInterceptor()(vendors.lastCtx,
		"/procurement.v1.Procurement/ListVendors", nil, nil, nil, invoker); err != nil {
		t.Fatalf("client interceptor: %v", err)
	}
	outMD, ok := metadata.FromOutgoingContext(intercepted)
	if !ok {
		t.Fatal("intercepted context carries no metadata")
	}
	tpValues := outMD.Get("traceparent")
	if len(tpValues) != 1 || tpValues[0][3:35] != inboundTraceID {
		t.Fatalf("outgoing traceparent = %v, want the request's trace %q", tpValues, inboundTraceID)
	}

	// 4. The response exposes the same id — legacy and OTel can never
	// disagree.
	if got := resp.Header.Get("X-Trace-Id"); got != inboundTraceID {
		t.Fatalf("X-Trace-Id response header = %q, want %q", got, inboundTraceID)
	}
}
