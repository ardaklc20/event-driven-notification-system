package provider_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
)

// permitted returns the test's context with a permit valid for a minute.
func permitted(t *testing.T) context.Context {
	t.Helper()
	return provider.WithPermit(t.Context(), provider.Permit{Deadline: time.Now().Add(time.Minute)})
}

// TestStartGate: the deadline check and the send start are one step before
// the transport (K42). A request past its deadline, or without a permit,
// never reaches the server and is not reported as a transport error.
func TestStartGate(t *testing.T) {
	var arrivals atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		arrivals.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	client := provider.NewClient(srv.URL, time.Second, 1)
	req := provider.Request{To: "+905551234567", Channel: "sms", Content: "c"}

	t.Run("deadline passed", func(t *testing.T) {
		started := false
		ctx := provider.WithPermit(t.Context(), provider.Permit{
			Deadline: time.Now().Add(-time.Millisecond),
			OnStart:  func(time.Time) { started = true },
		})
		_, err := client.Send(ctx, req)
		var transport *provider.TransportError
		if !errors.Is(err, provider.ErrStartDeadline) || errors.As(err, &transport) || started {
			t.Fatalf("err %v (transport error %v), OnStart called %v; want ErrStartDeadline only", err, transport != nil, started)
		}
	})
	t.Run("no permit", func(t *testing.T) {
		if _, err := client.Send(t.Context(), req); !errors.Is(err, provider.ErrNoPermit) {
			t.Fatalf("err %v, want ErrNoPermit", err)
		}
	})
	if n := arrivals.Load(); n != 0 {
		t.Fatalf("server received %d requests from refused sends, want 0", n)
	}

	t.Run("before the deadline", func(t *testing.T) {
		var starts []time.Time
		before := time.Now()
		ctx := provider.WithPermit(t.Context(), provider.Permit{
			Deadline: time.Now().Add(time.Second),
			OnStart:  func(at time.Time) { starts = append(starts, at) },
		})
		resp, err := client.Send(ctx, req)
		after := time.Now()
		if err != nil || resp.StatusCode != http.StatusAccepted || arrivals.Load() != 1 {
			t.Fatalf("resp %+v, err %v, %d arrivals; want one 202", resp, err, arrivals.Load())
		}
		if len(starts) != 1 || starts[0].Before(before) || starts[0].After(after) {
			t.Fatalf("starts %v, want one within the call", starts)
		}
	})
}
