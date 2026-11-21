package provider

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/ardaklc20/event-driven-notification-system/internal/tracing"
)

// ErrStartDeadline means the send did not start: the permit's start deadline
// had passed when the request reached the start gate. The request never
// reached the HTTP transport (K42).
var ErrStartDeadline = errors.New("start deadline missed, request not sent")

// ErrNoPermit means a request reached the start gate without a permit. Every
// send needs one (K42), so this is a programming error.
var ErrNoPermit = errors.New("request has no start permit")

// Permit is what a send needs to start (K42).
type Permit struct {
	// Deadline is the permit's start deadline, read from the worker's
	// monotonic clock. A send starts only before it.
	Deadline time.Time
	// OnStart, when set, is called with the send start after the check has
	// passed and before the request is handed to the HTTP transport. The
	// worker writes its start line there.
	OnStart func(start time.Time)
}

type permitKey struct{}

// WithPermit returns a context that carries p to the start gate.
func WithPermit(ctx context.Context, p Permit) context.Context {
	return context.WithValue(ctx, permitKey{}, p)
}

// startGate wraps the HTTP transport. Its RoundTrip is the last point before
// the request reaches the transport, so the deadline check and the counted
// send start are one step there (K42): the clock is read once, compared with
// the deadline, and that reading is the send start. A pause after it delays
// the network send only; it shows in the arrivals, not in the starts.
type startGate struct {
	next http.RoundTripper
}

func (g startGate) RoundTrip(req *http.Request) (*http.Response, error) {
	p, ok := req.Context().Value(permitKey{}).(Permit)
	if !ok {
		closeBody(req)
		return nil, ErrNoPermit
	}
	start := time.Now()
	if !start.Before(p.Deadline) {
		closeBody(req)
		return nil, ErrStartDeadline
	}
	if p.OnStart != nil {
		p.OnStart(start)
	}
	return g.next.RoundTrip(req)
}

// closeBody closes the request body when the gate does not pass the request
// on: a RoundTripper must close it, even on errors.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// newTransport builds the client's transport. Every request passes the start
// gate, which wraps the HTTP transport directly (K42). With tracing on,
// otelhttp wraps the gate from outside (K27): the client span starts and the
// traceparent header is set before the deadline check, never between the
// counted start and the transport.
func newTransport(inner http.RoundTripper, tr *tracing.Provider) http.RoundTripper {
	var rt http.RoundTripper = startGate{next: inner}
	if tr.Enabled() {
		rt = otelhttp.NewTransport(rt,
			otelhttp.WithTracerProvider(tr.TracerProvider()),
			otelhttp.WithPropagators(tr.Propagator()),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return "provider " + r.Method }),
		)
	}
	return rt
}
