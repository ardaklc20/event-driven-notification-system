package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m))
}

func newServer(t *testing.T, readiness *httpapi.Readiness, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	deps := httpapi.Deps{
		Logger:    slog.New(slog.DiscardHandler),
		Readiness: readiness,
	}
	if pool != nil {
		deps.Checks = []httpapi.Check{{
			Name: "database",
			Run:  func(ctx context.Context) error { return postgres.Ping(ctx, pool) },
		}}
	}
	srv := httptest.NewServer(httpapi.New(deps))
	t.Cleanup(srv.Close)
	return srv
}

// reply is a response read in full, with its body already closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

func do(t *testing.T, method, rawURL string) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, body
}

// TestRouter sends requests through the real router (K16).
func TestRouter(t *testing.T) {
	srv := newServer(t, &httpapi.Readiness{}, nil)

	resp, _ := do(t, http.MethodGet, srv.URL+"/no-such-path")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path: status %d, want 404", resp.StatusCode)
	}

	resp, _ = do(t, http.MethodPost, srv.URL+"/healthz")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: status %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("POST /healthz: Allow = %q, want it to list GET", allow)
	}
}

type problem struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// TestHealthEndpoints checks liveness and readiness with the database
// reachable and with it refusing connections (R16, K30).
func TestHealthEndpoints(t *testing.T) {
	t.Run("database reachable", func(t *testing.T) {
		srv := newServer(t, &httpapi.Readiness{}, testdb.New(t))

		resp, _ := do(t, http.MethodGet, srv.URL+"/healthz")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/healthz: status %d, want 200", resp.StatusCode)
		}

		resp, body := do(t, http.MethodGet, srv.URL+"/readyz")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/readyz: status %d, want 200; body %s", resp.StatusCode, body)
		}
		var ready httpapi.ReadinessBody
		if err := json.Unmarshal(body, &ready); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if ready.Status != "ready" || ready.Checks["database"] != "pass" {
			t.Errorf("/readyz body = %s", body)
		}
	})

	t.Run("database refusing connections", func(t *testing.T) {
		srv := newServer(t, &httpapi.Readiness{}, refusingPool(t))

		resp, _ := do(t, http.MethodGet, srv.URL+"/healthz")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/healthz: status %d, want 200", resp.StatusCode)
		}

		resp, body := do(t, http.MethodGet, srv.URL+"/readyz")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("/readyz: status %d, want 503; body %s", resp.StatusCode, body)
		}
		var p problem
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(p.Errors) != 1 || p.Errors[0].Location != "checks.database" {
			t.Errorf("/readyz body = %s, want the database check named", body)
		}
	})
}

// refusingPool returns a pool pointed at a local port nothing listens on.
func refusingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	pool, err := postgres.NewPool(t.Context(), "postgres://u:p@"+addr+"/db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestReadyzDuringShutdown checks that /readyz answers 503 once shutdown has
// started, while liveness still answers 200 (K6, K30).
func TestReadyzDuringShutdown(t *testing.T) {
	readiness := &httpapi.Readiness{}
	srv := newServer(t, readiness, nil)

	resp, _ := do(t, http.MethodGet, srv.URL+"/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("before shutdown: status %d, want 200", resp.StatusCode)
	}

	readiness.StartShutdown()

	resp, body := do(t, http.MethodGet, srv.URL+"/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after shutdown started: status %d, want 503", resp.StatusCode)
	}
	var p problem
	if err := json.Unmarshal(body, &p); err != nil || p.Detail != "shutting down" {
		t.Errorf("body = %s, want detail \"shutting down\"", body)
	}
	resp, _ = do(t, http.MethodGet, srv.URL+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz during shutdown: status %d, want 200", resp.StatusCode)
	}
}

// TestHealthcheckCommand checks the exit status of `api healthcheck` against
// a server answering 200, then 503, then nothing (K31).
func TestHealthcheckCommand(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port := u.Port()

	var out bytes.Buffer
	if code := httpapi.Healthcheck(t.Context(), port, &out); code != 0 {
		t.Errorf("200: exit %d, want 0 (%s)", code, out.String())
	}

	status.Store(http.StatusServiceUnavailable)
	if code := httpapi.Healthcheck(t.Context(), port, &out); code != 1 {
		t.Errorf("503: exit %d, want 1", code)
	}

	srv.Close()
	if code := httpapi.Healthcheck(t.Context(), port, &out); code != 1 {
		t.Errorf("no server: exit %d, want 1", code)
	}
}

// TestOpenAPIUpToDate compares the generated document with the committed
// api/openapi.yaml, byte for byte (D4, K19).
func TestOpenAPIUpToDate(t *testing.T) {
	generated, err := httpapi.OpenAPIYAML()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	committed, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	if !bytes.Equal(generated, committed) {
		t.Fatal("api/openapi.yaml is out of date; run: go run ./cmd/api openapi > api/openapi.yaml")
	}
}
