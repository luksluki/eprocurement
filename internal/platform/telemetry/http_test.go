package telemetry

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// AC #3/#4 (edge): HTTPTrace must (a) keep an inbound traceparent as the
// trace root, (b) honor a bare legacy X-Trace-Id hex32 the same way, and
// (c) derive the REST-visible X-Trace-Id response header and the
// "trace_id" local FROM the span so legacy values can never disagree with
// Jaeger. The span context is installed via c.SetUserContext — the value
// forwardContext reads — asserted here by reading c.UserContext() inside
// the handler.
func TestHTTPTraceDerivesTraceIDFromSpan(t *testing.T) {
	const fromTraceparent = "0af7651916cd43dd8448eb211c80319c"
	const fromLegacyHeader = "1117651916cd43dd8448eb211c803199"

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"inbound traceparent is the trace root", map[string]string{
			"traceparent": "00-" + fromTraceparent + "-00f067aa0ba902b7-01"}},
		{"legacy X-Trace-Id hex32 anchors the same trace", map[string]string{
			"X-Trace-Id": fromLegacyHeader}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want string
			for k, v := range tc.headers {
				if k == "traceparent" {
					want = v[3:35]
				}
			}
			if want == "" {
				want = fromLegacyHeader
			}

			var spanTraceID, localTraceID string
			app := fiber.New()
			app.Use(HTTPTrace())
			app.Get("/api/v1/vendors", func(c *fiber.Ctx) error {
				if sc := spanContextOf(c.UserContext()); sc.IsValid() {
					spanTraceID = sc.TraceID().String()
				}
				localTraceID, _ = c.Locals("trace_id").(string)
				return c.JSON(fiber.Map{"ok": true})
			})

			req := httptest.NewRequest("GET", "/api/v1/vendors", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if spanTraceID != want {
				t.Fatalf("span trace id = %q, want %q", spanTraceID, want)
			}
			if localTraceID != want {
				t.Fatalf("trace_id local = %q, want %q (must be derived from the span)", localTraceID, want)
			}
			if got := resp.Header.Get("X-Trace-Id"); got != want {
				t.Fatalf("X-Trace-Id response header = %q, want %q", got, want)
			}
		})
	}
}

// No headers at all: a fresh trace id is minted and exposed the same way.
func TestHTTPTraceMintsFreshTraceID(t *testing.T) {
	var spanTraceID, localTraceID string
	app := fiber.New()
	app.Use(HTTPTrace())
	app.Get("/api/v1/vendors", func(c *fiber.Ctx) error {
		if sc := spanContextOf(c.UserContext()); sc.IsValid() {
			spanTraceID = sc.TraceID().String()
		}
		localTraceID, _ = c.Locals("trace_id").(string)
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/api/v1/vendors", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if spanTraceID == "" {
		t.Fatal("no span context in c.UserContext(); SDK root span missing")
	}
	if localTraceID != spanTraceID {
		t.Fatalf("trace_id local %q != span trace id %q", localTraceID, spanTraceID)
	}
	if got := resp.Header.Get("X-Trace-Id"); got != spanTraceID {
		t.Fatalf("X-Trace-Id = %q, want span trace id %q", got, spanTraceID)
	}
}

// The compose healthcheck endpoint stays span-free.
func TestHTTPTraceSkipsHealthz(t *testing.T) {
	var sawSpan bool
	app := fiber.New()
	app.Use(HTTPTrace())
	app.Get("/healthz", func(c *fiber.Ctx) error {
		sawSpan = spanContextOf(c.UserContext()).IsValid()
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/healthz", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if sawSpan {
		t.Fatal("/healthz must not open a span (healthcheck noise)")
	}
}
