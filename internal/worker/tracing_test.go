package worker_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

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

// TestRetryAttemptsLinked: each attempt is its own trace, each linked to the
// same stored context (K27).
func TestRetryAttemptsLinked(t *testing.T) {
	pool := testdb.New(t)
	s, rec := tracedSettings(t)
	id := insertTraced(t, pool)
	_, srv := newProvider(t, sequence(http.StatusServiceUnavailable, http.StatusAccepted))
	w := startWorker(t, pool, srv.URL+"/", s)
	waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatal(err)
	}
	attempts := spansOf(t, rec, trace.SpanKindConsumer, 2)
	for _, a := range attempts {
		wantLinkedToStored(t, a)
	}
	if attempts[0].SpanContext().TraceID() == attempts[1].SpanContext().TraceID() {
		t.Fatal("both attempts share one trace ID, want one trace each")
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
