package tracing

import (
	"context"
	"net/url"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// URL attributes that otelhttp fills with the request's full URL. The
// provider URL can hold a secret in its path, as a webhook.site token does,
// so only its scheme and host leave the process.
var urlKeys = map[attribute.Key]bool{"url.full": true, "http.url": true}

// redactingExporter replaces the URL attributes of every span it exports.
type redactingExporter struct {
	next sdktrace.SpanExporter
}

func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = redactedSpan{ReadOnlySpan: s}
	}
	return e.next.ExportSpans(ctx, out)
}

func (e redactingExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

// redactedSpan is a span whose URL attributes keep only scheme and host.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
}

func (s redactedSpan) Attributes() []attribute.KeyValue {
	attrs := s.ReadOnlySpan.Attributes()
	out := make([]attribute.KeyValue, len(attrs))
	for i, kv := range attrs {
		if urlKeys[kv.Key] {
			kv = attribute.String(string(kv.Key), RedactURL(kv.Value.AsString()))
		}
		out[i] = kv
	}
	return out
}

// RedactURL returns scheme://host/[redacted] for a URL, or [redacted] when it
// does not parse.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	return u.Scheme + "://" + u.Host + "/[redacted]"
}
