package httpapi_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/internal/tracing"
)

// tracedServer is the API with tracing on, its spans kept in memory.
func tracedServer(t *testing.T, pool *pgxpool.Pool) (string, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Logger:        slog.New(slog.DiscardHandler),
		Readiness:     &httpapi.Readiness{},
		Notifications: service.NewNotifications(postgres.NewStore(pool), domain.DefaultLimits()),
		Tracing:       tracing.NewWithProvider(tp),
	}))
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

// serverSpan waits for the one ended server span; otelhttp ends it after the
// handler returns, which can be just after the client has the response.
func serverSpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var servers []sdktrace.ReadOnlySpan
		for _, s := range rec.Ended() {
			if s.SpanKind() == trace.SpanKindServer {
				servers = append(servers, s)
			}
		}
		if len(servers) == 1 {
			return servers[0]
		}
		if len(servers) > 1 || time.Now().After(deadline) {
			t.Fatalf("%d server spans, want 1", len(servers))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func storedTraceparents(t *testing.T, pool *pgxpool.Pool) []*string {
	t.Helper()
	r, err := pool.Query(t.Context(), `SELECT traceparent FROM notifications ORDER BY batch_position NULLS FIRST, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []*string
	for r.Next() {
		var tp *string
		if err := r.Scan(&tp); err != nil {
			t.Fatal(err)
		}
		out = append(out, tp)
	}
	return out
}

const clientTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// TestStoredTraceContext: the stored traceparent is the API's own server
// span, a child of the client's span when the client sent one (K27).
func TestStoredTraceContext(t *testing.T) {
	for name, header := range map[string]http.Header{
		"with header":    {"Traceparent": {clientTraceparent}},
		"without header": nil,
	} {
		t.Run(name, func(t *testing.T) {
			pool := testdb.New(t)
			base, rec := tracedServer(t, pool)
			if resp, raw := post(t, base, smsBody, header); resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status %d %s", resp.StatusCode, raw)
			}
			span := serverSpan(t, rec)
			stored := storedTraceparents(t, pool)
			if len(stored) != 1 || stored[0] == nil {
				t.Fatalf("stored %v, want one traceparent", stored)
			}
			want := "00-" + span.SpanContext().TraceID().String() + "-" + span.SpanContext().SpanID().String() + "-01"
			if *stored[0] != want {
				t.Fatalf("stored %s, want the server span %s", *stored[0], want)
			}
			if header != nil {
				parts := strings.Split(clientTraceparent, "-")
				if span.SpanContext().TraceID().String() != parts[1] {
					t.Fatalf("trace ID %s, want the client's %s", span.SpanContext().TraceID(), parts[1])
				}
				if span.SpanContext().SpanID().String() == parts[2] || span.Parent().SpanID().String() != parts[2] {
					t.Fatalf("server span %s with parent %s; want a new span whose parent is the client's %s",
						span.SpanContext().SpanID(), span.Parent().SpanID(), parts[2])
				}
			} else if span.Parent().IsValid() {
				t.Fatalf("server span has parent %v, want a root span", span.Parent())
			}
		})
	}
}

// TestBatchStoredTraceContext: every item of a batch stores the batch
// request's server span (K27).
func TestBatchStoredTraceContext(t *testing.T) {
	pool := testdb.New(t)
	base, rec := tracedServer(t, pool)
	if resp, raw := postBatch(t, base, batchJSON(t, mixedItems(5)), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	span := serverSpan(t, rec)
	want := "00-" + span.SpanContext().TraceID().String() + "-" + span.SpanContext().SpanID().String() + "-01"
	stored := storedTraceparents(t, pool)
	if len(stored) != 5 {
		t.Fatalf("%d rows, want 5", len(stored))
	}
	for i, tp := range stored {
		if tp == nil || *tp != want {
			t.Fatalf("item %d stored %v, want %s", i, tp, want)
		}
	}
}

// TestNoTraceContextWhenOff: with tracing off nothing is stored, even when
// the client sends a traceparent, and the request is accepted as usual.
func TestNoTraceContextWhenOff(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())
	resp, raw := post(t, base, smsBody, http.Header{"Traceparent": {clientTraceparent}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d %s, want 202", resp.StatusCode, raw)
	}
	if stored := storedTraceparents(t, pool); len(stored) != 1 || stored[0] != nil {
		t.Fatalf("stored %v, want one null traceparent", stored)
	}
}
