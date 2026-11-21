// Package tracing sets up OpenTelemetry for the API and the worker (K27).
// Tracing is off unless an OTLP endpoint is set; a collector that is down or
// silent never blocks a request or a delivery.
package tracing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	// ExportTimeout bounds one export to the collector. It is shorter than
	// ShutdownTimeout, so a collector that accepts the connection and never
	// answers cannot hold a process past its shutdown budget.
	ExportTimeout = 2 * time.Second
	// ShutdownTimeout bounds the final flush at shutdown. It fits in the
	// time Compose leaves after each process's own shutdown budget (K6).
	ShutdownTimeout = 3 * time.Second
	// tracesPath is the OTLP/HTTP signal path appended to the base endpoint,
	// as the exporter does for OTEL_EXPORTER_OTLP_ENDPOINT.
	tracesPath = "/v1/traces"
)

// Config is what tracing needs from the process's configuration (K32).
type Config struct {
	// Endpoint is OTEL_EXPORTER_OTLP_ENDPOINT, a base URL such as
	// http://jaeger:4318; empty turns tracing off.
	Endpoint string
	// ServiceName is OTEL_SERVICE_NAME.
	ServiceName string
}

// Provider holds the tracer provider and the propagator of one process.
type Provider struct {
	sdk        *sdktrace.TracerProvider // nil when off
	exporter   sdktrace.SpanExporter    // nil when off
	propagator propagation.TextMapPropagator
}

// New returns the process's provider. With an empty endpoint no exporter is
// created and every tracer is a no-op.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	p := &Provider{propagator: propagation.TraceContext{}}
	if cfg.Endpoint == "" {
		return p, nil
	}
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(strings.TrimRight(cfg.Endpoint, "/")+tracesPath),
		otlptracehttp.WithTimeout(ExportTimeout),
		// One try per batch: a retry would outlive the export timeout.
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res := resource.NewSchemaless(semconv.ServiceName(cfg.ServiceName))
	p.exporter = redactingExporter{next: exporter}
	p.sdk = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		// The batch processor drops spans when its queue is full instead of
		// blocking the caller.
		sdktrace.WithBatcher(p.exporter, sdktrace.WithExportTimeout(ExportTimeout)),
	)
	return p, nil
}

// NewWithProvider wraps a tracer provider a test built, for example with an
// in-memory recorder.
func NewWithProvider(tp *sdktrace.TracerProvider) *Provider {
	return &Provider{sdk: tp, propagator: propagation.TraceContext{}}
}

// Enabled reports whether spans are recorded.
func (p *Provider) Enabled() bool { return p != nil && p.sdk != nil }

// Exporter returns the OTLP exporter, or nil when tracing is off.
func (p *Provider) Exporter() sdktrace.SpanExporter {
	if p == nil {
		return nil
	}
	return p.exporter
}

// TracerProvider returns the provider, a no-op one when tracing is off.
func (p *Provider) TracerProvider() trace.TracerProvider {
	if !p.Enabled() {
		return noop.NewTracerProvider()
	}
	return p.sdk
}

// Propagator returns the W3C trace context propagator.
func (p *Provider) Propagator() propagation.TextMapPropagator {
	if p == nil {
		return propagation.TraceContext{}
	}
	return p.propagator
}

// Shutdown flushes the spans still queued and stops the exporter, within
// ctx. It does nothing when tracing is off.
func (p *Provider) Shutdown(ctx context.Context) error {
	if !p.Enabled() {
		return nil
	}
	return p.sdk.Shutdown(ctx)
}

// Traceparent returns the W3C traceparent of the span in ctx, or "" when ctx
// holds no valid span context (K27).
func Traceparent(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%s", sc.TraceID(), sc.SpanID(), sc.TraceFlags())
}

// SpanContext parses a stored traceparent; the result is invalid for "" or a
// malformed value.
func SpanContext(traceparent string) trace.SpanContext {
	if traceparent == "" {
		return trace.SpanContext{}
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(),
		propagation.MapCarrier{"traceparent": traceparent})
	return trace.SpanContextFromContext(ctx)
}
