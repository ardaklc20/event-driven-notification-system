package tracing_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ardaklc20/event-driven-notification-system/internal/tracing"
)

// collector records what an OTLP/HTTP exporter posts to it.
type collector struct {
	mu       sync.Mutex
	paths    []string
	types    []string
	payloads [][]byte
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.paths = append(c.paths, r.URL.Path)
	c.types = append(c.types, r.Header.Get("Content-Type"))
	c.payloads = append(c.payloads, body)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// TestTracingConfig: without an endpoint no exporter exists and tracers are
// no-ops; with one, spans are posted over OTLP/HTTP to its /v1/traces, with
// the provider URL's path kept out (K27).
func TestTracingConfig(t *testing.T) {
	t.Run("endpoint empty", func(t *testing.T) {
		p, err := tracing.New(t.Context(), tracing.Config{ServiceName: "notification-api"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Enabled() || p.Exporter() != nil {
			t.Fatalf("enabled %v, exporter %v; want off with no exporter", p.Enabled(), p.Exporter())
		}
		if _, ok := p.TracerProvider().(noop.TracerProvider); !ok {
			t.Fatalf("tracer provider %T, want the no-op one", p.TracerProvider())
		}
		_, span := p.TracerProvider().Tracer("t").Start(context.Background(), "x")
		if span.SpanContext().IsValid() {
			t.Fatal("a span with a valid context while tracing is off")
		}
		if err := p.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("endpoint set", func(t *testing.T) {
		c := &collector{}
		srv := httptest.NewServer(c)
		defer srv.Close()

		p, err := tracing.New(t.Context(), tracing.Config{Endpoint: srv.URL + "/", ServiceName: "notification-worker"})
		if err != nil {
			t.Fatal(err)
		}
		if !p.Enabled() || p.Exporter() == nil {
			t.Fatal("tracing is off with an endpoint set")
		}
		const secret = "263d1d46-secret-token-path"
		_, span := p.TracerProvider().Tracer("t").Start(context.Background(), "provider POST")
		span.SetAttributes(attribute.String("url.full", "https://webhook.site/"+secret))
		span.End()

		ctx, cancel := context.WithTimeout(context.Background(), tracing.ShutdownTimeout)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.payloads) != 1 || c.paths[0] != "/v1/traces" || c.types[0] != "application/x-protobuf" {
			t.Fatalf("collector got %d posts, paths %v, types %v; want one protobuf post to /v1/traces", len(c.payloads), c.paths, c.types)
		}
		body := c.payloads[0]
		if !bytes.Contains(body, []byte("notification-worker")) || !bytes.Contains(body, []byte("provider POST")) {
			t.Fatal("the export holds neither the service name nor the span")
		}
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("the export holds the URL's path")
		}
		if !bytes.Contains(body, []byte("https://webhook.site/[redacted]")) {
			t.Fatal("the export does not hold the redacted URL")
		}
	})

	t.Run("redaction", func(t *testing.T) {
		for raw, want := range map[string]string{
			"https://webhook.site/263d1d46-aaaa?x=1": "https://webhook.site/[redacted]",
			"http://mockprovider:8081/":              "http://mockprovider:8081/[redacted]",
			"not a url":                              "[redacted]",
		} {
			if got := tracing.RedactURL(raw); got != want {
				t.Errorf("RedactURL(%q) = %q, want %q", raw, got, want)
			}
		}
	})

	t.Run("traceparent round trip", func(t *testing.T) {
		const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
		sc := tracing.SpanContext(tp)
		if !sc.IsValid() || sc.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || sc.SpanID().String() != "b7ad6b7169203331" {
			t.Fatalf("parsed %v", sc)
		}
		if tracing.SpanContext("").IsValid() || tracing.SpanContext("garbage").IsValid() {
			t.Fatal("an empty or malformed traceparent parsed as valid")
		}
	})
}
