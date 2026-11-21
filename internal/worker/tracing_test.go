package worker_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/internal/tracing"
)

// storedTP is the trace context the API would have stored.
const storedTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// tracedSettings are the defaults with tracing on, spans kept in memory.
func tracedSettings(t *testing.T) (settings, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	s := defaults()
	s.cfg.Tracing = tracing.NewWithProvider(tp)
	return s, rec
}

func insertTraced(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, traceparent)
		 VALUES ('+905551234567', 'sms', 'traced', 'normal', 'corr-traced', $1) RETURNING id::text`, storedTP).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// spansOf waits until n spans of kind have ended and returns them.
func spansOf(t *testing.T, rec *tracetest.SpanRecorder, kind trace.SpanKind, n int) []sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var out []sdktrace.ReadOnlySpan
		for _, s := range rec.Ended() {
			if s.SpanKind() == kind {
				out = append(out, s)
			}
		}
		if len(out) >= n || time.Now().After(deadline) {
			if len(out) != n {
				t.Fatalf("%d spans of kind %v, want %d", len(out), kind, n)
			}
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func wantLinkedToStored(t *testing.T, span sdktrace.ReadOnlySpan) {
	t.Helper()
	parts := strings.Split(storedTP, "-")
	if span.Parent().IsValid() {
		t.Fatalf("attempt span has parent %v, want none", span.Parent())
	}
	links := span.Links()
	if len(links) != 1 || links[0].SpanContext.TraceID().String() != parts[1] || links[0].SpanContext.SpanID().String() != parts[2] {
		t.Fatalf("links %+v, want one link to %s", links, storedTP)
	}
	if span.SpanContext().TraceID().String() == parts[1] {
		t.Fatal("the attempt span is in the request's trace, want a trace of its own")
	}
}

// TestAttemptSpanLink: an attempt is a root span with one link to the stored
// context (K27).
func TestAttemptSpanLink(t *testing.T) {
	pool := testdb.New(t)
	s, rec := tracedSettings(t)
	id := insertTraced(t, pool)
	_, srv := newProvider(t, accepted(validAccepted))
	w := startWorker(t, pool, srv.URL+"/", s)
	waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatal(err)
	}
	wantLinkedToStored(t, spansOf(t, rec, trace.SpanKindConsumer, 1)[0])
}

// TestRetryAttemptsLinked: provider attempts and a claim whose start deadline
// is missed each have their own trace linked to the same stored context (K27).
func TestRetryAttemptsLinked(t *testing.T) {
	pool := testdb.New(t)
	s, rec := tracedSettings(t)
	var pause sync.Once
	s.hooks.BeforeGate = func(domain.Claim) {
		pause.Do(func() { time.Sleep(2 * s.cfg.PermitGrace) })
	}
	id := insertTraced(t, pool)
	provider, srv := newProvider(t, sequence(http.StatusServiceUnavailable, http.StatusAccepted))
	w := startWorker(t, pool, srv.URL+"/", s)
	waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatal(err)
	}
	arrivals := provider.all()
	result := readRow(t, pool, id)
	if len(arrivals) != 2 || result.attempts != 2 {
		t.Fatalf("%d POSTs, row %+v; want exactly two provider attempts", len(arrivals), result)
	}
	seen := make(map[trace.TraceID]bool)
	byClass := make(map[string]trace.TraceID)
	missedStarts := 0
	// A missed start deadline also produces a consumer span, but returns its
	// attempt without a POST. Check every link and allow only that specific
	// extra span; the two actual provider attempts must still be distinct.
	for _, span := range rec.Ended() {
		if span.SpanKind() != trace.SpanKindConsumer {
			continue
		}
		wantLinkedToStored(t, span)
		traceID := span.SpanContext().TraceID()
		if seen[traceID] {
			t.Fatal("consumer spans share a trace ID, want one trace each")
		}
		seen[traceID] = true
		class := ""
		for _, attr := range span.Attributes() {
			if attr.Key == "provider.class" {
				class = attr.Value.AsString()
			}
		}
		switch class {
		case "temporary", "accepted":
			if _, exists := byClass[class]; exists {
				t.Fatalf("multiple consumer spans with provider class %q", class)
			}
			byClass[class] = traceID
		default:
			status := span.Status()
			if class != "" || status.Code != codes.Error || status.Description != "start deadline missed" {
				t.Fatalf("unexpected consumer span: class %q, status %+v", class, status)
			}
			missedStarts++
		}
	}
	if missedStarts == 0 {
		t.Fatal("no consumer span recorded the forced start deadline miss")
	}
	for i, class := range []string{"temporary", "accepted"} {
		traceID, exists := byClass[class]
		parts := strings.Split(arrivals[i].header.Get("Traceparent"), "-")
		if !exists || len(parts) != 4 || parts[1] != traceID.String() {
			t.Fatalf("POST %d traceparent %q does not match its %s consumer span", i+1, arrivals[i].header.Get("Traceparent"), class)
		}
	}
}

// TestProviderTraceparent: the provider receives a traceparent from the
// attempt's trace, whose parent is the client span under the attempt span
// (K27).
func TestProviderTraceparent(t *testing.T) {
	pool := testdb.New(t)
	s, rec := tracedSettings(t)
	id := insertTraced(t, pool)
	recorder, srv := newProvider(t, accepted(validAccepted))
	w := startWorker(t, pool, srv.URL+"/", s)
	waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatal(err)
	}
	attempt := spansOf(t, rec, trace.SpanKindConsumer, 1)[0]
	client := spansOf(t, rec, trace.SpanKindClient, 1)[0]

	arrivals := recorder.all()
	if len(arrivals) != 1 {
		t.Fatalf("%d POSTs, want 1", len(arrivals))
	}
	header := arrivals[0].header.Get("Traceparent")
	parts := strings.Split(header, "-")
	if len(parts) != 4 {
		t.Fatalf("traceparent %q, want a W3C value", header)
	}
	if parts[1] != attempt.SpanContext().TraceID().String() {
		t.Fatalf("traceparent trace %s, want the attempt's %s", parts[1], attempt.SpanContext().TraceID())
	}
	if parts[2] != client.SpanContext().SpanID().String() {
		t.Fatalf("traceparent parent %s, want the client span %s", parts[2], client.SpanContext().SpanID())
	}
	if client.Parent().SpanID() != attempt.SpanContext().SpanID() {
		t.Fatalf("client span's parent %s, want the attempt span %s", client.Parent().SpanID(), attempt.SpanContext().SpanID())
	}
}
