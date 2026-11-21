package e2e_test

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/tracing"
)

// silentCollector accepts connections and never answers.
func silentCollector(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "http://" + ln.Addr().String()
}

// closedEndpoint is an address where nothing listens.
func closedEndpoint(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// TestTracingCollectorDown: a collector that refuses connections or never
// answers neither stops delivery nor holds either process past its shutdown
// budget (K27).
func TestTracingCollectorDown(t *testing.T) {
	for name, endpoint := range map[string]func(*testing.T) string{
		"nothing listens":  closedEndpoint,
		"collector silent": silentCollector,
	} {
		t.Run(name, func(t *testing.T) {
			url := endpoint(t)
			api, err := tracing.New(t.Context(), tracing.Config{Endpoint: url, ServiceName: "notification-api"})
			if err != nil {
				t.Fatal(err)
			}
			wrk, err := tracing.New(t.Context(), tracing.Config{Endpoint: url, ServiceName: "notification-worker"})
			if err != nil {
				t.Fatal(err)
			}
			s := newStackWithTracing(t, func(w http.ResponseWriter, _ *http.Request) { accept(w) }, api, wrk)
			s.startWorker(t)

			r := s.create(t, `{"recipient":"+905551234567","channel":"sms","content":"traced"}`, nil)
			if r.status != http.StatusAccepted {
				t.Fatalf("create: status %d %s", r.status, r.body)
			}
			s.waitSent(t, idOf(t, r), 30*time.Second)

			if took := s.stopWorker(); took > 10*time.Second {
				t.Fatalf("worker stopped after %s, over its 10 s budget", took)
			}
			for proc, p := range map[string]*tracing.Provider{"api": api, "worker": wrk} {
				ctx, cancel := context.WithTimeout(context.Background(), tracing.ShutdownTimeout)
				start := time.Now()
				_ = p.Shutdown(ctx) // an error is expected: nothing receives the spans
				took := time.Since(start)
				cancel()
				if took > tracing.ShutdownTimeout+500*time.Millisecond {
					t.Fatalf("%s tracing flush took %s, over its %s budget", proc, took, tracing.ShutdownTimeout)
				}
				t.Logf("%s tracing flush returned after %s", proc, took.Round(time.Millisecond))
			}
		})
	}
}
