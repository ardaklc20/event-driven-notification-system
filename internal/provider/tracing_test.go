package provider_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
	"github.com/ardaklc20/event-driven-notification-system/internal/tracing"
)

// countingTransport stands in for http.Transport and records what reaches it.
type countingTransport struct {
	calls       atomic.Int32
	traceparent atomic.Value
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	c.traceparent.Store(r.Header.Get("Traceparent"))
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
}

func permitRequest(t *testing.T, deadline time.Time) *http.Request {
	t.Helper()
	ctx := provider.WithPermit(context.Background(), provider.Permit{Deadline: deadline})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://provider.test/", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestTransportOrder: with tracing on, otelhttp is the outer RoundTripper,
// it wraps the start gate, and the gate wraps the transport directly; a
// passed deadline stops the request at the gate (K27, K42).
func TestTransportOrder(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	inner := &countingTransport{}

	rt := provider.NewTransport(inner, tracing.NewWithProvider(tp))
	outer, ok := rt.(*otelhttp.Transport)
	if !ok {
		t.Fatalf("outer RoundTripper is %T, want *otelhttp.Transport", rt)
	}
	gate := reflect.ValueOf(outer).Elem().FieldByName("rt").Elem()
	if gate.Type() != provider.StartGateType {
		t.Fatalf("otelhttp wraps %v, want the start gate", gate.Type())
	}
	next := gate.FieldByName("next").Elem()
	if next.Type() != reflect.TypeFor[*countingTransport]() || next.Pointer() != reflect.ValueOf(inner).Pointer() {
		t.Fatalf("the gate wraps %v, want the transport itself", next.Type())
	}

	// A deadline already passed: refused at the gate, never at the transport.
	refused, err := rt.RoundTrip(permitRequest(t, time.Now().Add(-time.Second)))
	if refused != nil {
		_ = refused.Body.Close()
	}
	if !errors.Is(err, provider.ErrStartDeadline) || inner.calls.Load() != 0 {
		t.Fatalf("err %v with %d transport calls, want ErrStartDeadline and none", err, inner.calls.Load())
	}

	// A deadline ahead: the transport gets the request with the client
	// span's traceparent, set before the gate.
	resp, err := rt.RoundTrip(permitRequest(t, time.Now().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if inner.calls.Load() != 1 || inner.traceparent.Load() == "" {
		t.Fatalf("%d transport calls, traceparent %q; want one call with a traceparent", inner.calls.Load(), inner.traceparent.Load())
	}

	// Tracing off: the gate is the outer RoundTripper.
	if off := provider.NewTransport(inner, nil); reflect.TypeOf(off) != provider.StartGateType {
		t.Fatalf("without tracing the outer RoundTripper is %T, want the start gate", off)
	}
}
