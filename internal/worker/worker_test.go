package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/internal/worker"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m))
}

// settings are a worker's settings in these tests.
type settings struct {
	httpTimeout time.Duration
	cfg         worker.Config
	// hooks stop a delivery at a chosen point (crash simulation).
	hooks worker.Hooks
	// wrap, when set, wraps the store the worker uses.
	wrap func(worker.Store) worker.Store
	// id is the worker_id in the logs; "test-worker" when empty.
	id string
}

// defaults retry at once (zero backoff), so attempt sequences run fast.
func defaults() settings {
	return settings{
		httpTimeout: 5 * time.Second,
		cfg: worker.Config{
			Profile:          provider.ProfileStandard,
			LeaseDuration:    15 * time.Second,
			ResultBudget:     5 * time.Second,
			InFlight:         4,
			IdleWait:         20 * time.Millisecond,
			ShutdownBudget:   10 * time.Second,
			MaxAttempts:      5,
			BackoffBase:      0,
			BackoffCap:       0,
			RecoveryInterval: 100 * time.Millisecond,
			RecoveryBatch:    100,
			PermitInterval:   11 * time.Millisecond,
			PermitGrace:      100 * time.Millisecond,
			// A threshold no other test reaches: only the breaker tests
			// open the breaker (K34).
			Breaker: domain.BreakerPolicy{Threshold: 1000, Cooldown: time.Second, MaxCooldown: time.Second},
		},
	}
}

// logBuffer collects JSON log lines from several goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range bytes.Lines(b.buf.Bytes()) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

type running struct {
	cancel context.CancelFunc
	done   chan error
	logs   *logBuffer
}

// startWorker runs a worker against pool and providerURL until stop or the
// end of the test.
func startWorker(t *testing.T, pool *pgxpool.Pool, providerURL string, s settings) *running {
	t.Helper()
	logs := &logBuffer{}
	id := s.id
	if id == "" {
		id = "test-worker"
	}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})).
		With(slog.String("worker_id", id))
	client := provider.NewClient(providerURL, s.httpTimeout, s.cfg.InFlight, provider.WithTracing(s.cfg.Tracing))
	var store worker.Store = postgres.NewStore(pool)
	if s.wrap != nil {
		store = s.wrap(store)
	}
	w := worker.New(store, client, logger, s.cfg)
	worker.SetHooks(w, s.hooks)

	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan error, 1), logs: logs}
	go func() { r.done <- w.Run(ctx) }()
	t.Cleanup(func() { _ = r.stop(t) })
	return r
}

// stop requests the stop and waits at most 15 seconds for Run to return.
func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err // later calls see the same result
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not return within 15 seconds of the stop")
		return nil
	}
}

func insertQueued(t *testing.T, pool *pgxpool.Pool, n int, correlationID string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
		 SELECT '+905551234567', 'sms', 'hello', 'normal', $2
		 FROM generate_series(1, $1)
		 RETURNING id::text`, n, correlationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

type row struct {
	status    string
	attempts  int
	token     *string
	expires   *time.Time
	sentAt    *time.Time
	messageID *string
	timestamp *time.Time
	lastError *string
}

func readRow(t *testing.T, pool *pgxpool.Pool, id string) row {
	t.Helper()
	var r row
	err := pool.QueryRow(context.Background(),
		`SELECT status::text, attempts, lease_token::text, lease_expires_at, sent_at,
		        provider_message_id, provider_timestamp, last_error
		 FROM notifications WHERE id = $1::uuid`, id).Scan(
		&r.status, &r.attempts, &r.token, &r.expires, &r.sentAt, &r.messageID, &r.timestamp, &r.lastError)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return r
}

// waitStatus polls the row until it has status want, for at most within.
func waitStatus(t *testing.T, pool *pgxpool.Pool, id, want string, within time.Duration) row {
	t.Helper()
	return waitRow(t, pool, id, within, want, func(r row) bool { return r.status == want })
}

// waitRow polls the row until ok holds, for at most within.
func waitRow(t *testing.T, pool *pgxpool.Pool, id string, within time.Duration, what string, ok func(row) bool) row {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r := readRow(t, pool, id)
		if ok(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %s after %s: %+v, want %s", id, within, r, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// arrival is one request the test provider received.
type arrival struct {
	at     time.Time
	header http.Header
	body   []byte
}

// recorder records every request before handing it to answer.
type recorder struct {
	mu       sync.Mutex
	arrivals []arrival
	answer   http.HandlerFunc
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	at := time.Now()
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body)) // answer may read it too
	rec.mu.Lock()
	rec.arrivals = append(rec.arrivals, arrival{at: at, header: r.Header.Clone(), body: body})
	rec.mu.Unlock()
	rec.answer(w, r)
}

func (rec *recorder) all() []arrival {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]arrival(nil), rec.arrivals...)
}

func newProvider(t *testing.T, answer http.HandlerFunc) (*recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{answer: answer}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return rec, srv
}

func accepted(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, body)
	}
}

func countLevel(records []map[string]any, level string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["level"] == level {
			out = append(out, r)
		}
	}
	return out
}

// TestDeliverySent: a 202 with a valid body ends as sent with both provider
// fields (R12, R13, K15, A2).
func TestDeliverySent(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-sent")[0]
	rec, srv := newProvider(t, accepted(`{"messageId":"m-123","status":"accepted","timestamp":"2026-11-20T10:30:00Z"}`))

	w := startWorker(t, pool, srv.URL+"/", defaults())
	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}

	arrivals := rec.all()
	if len(arrivals) != 1 {
		t.Fatalf("provider got %d POSTs, want 1", len(arrivals))
	}
	a := arrivals[0]
	var body map[string]any
	if err := json.Unmarshal(a.body, &body); err != nil {
		t.Fatalf("request body %s: %v", a.body, err)
	}
	if len(body) != 3 || body["to"] != "+905551234567" || body["channel"] != "sms" || body["content"] != "hello" {
		t.Errorf("request body = %s, want exactly {to, channel, content}", a.body)
	}
	if ct := a.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := a.header.Get(provider.CorrelationIDHeader); got != "corr-sent" {
		t.Errorf("X-Correlation-ID = %q, want corr-sent", got)
	}

	want := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	if r.sentAt == nil || r.messageID == nil || *r.messageID != "m-123" || r.timestamp == nil || !r.timestamp.Equal(want) {
		t.Errorf("row = %+v, want sent_at, messageId m-123 and the provider timestamp", r)
	}
	if r.token != nil || r.expires != nil || r.attempts != 1 {
		t.Errorf("row = %+v, want the lease cleared and attempts 1", r)
	}
}

// TestAcceptedWithBrokenBody: a 202 whose body is not JSON still ends as sent,
// with the provider fields empty and one warning (K15).
func TestAcceptedWithBrokenBody(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-broken")[0]
	rec, srv := newProvider(t, accepted(`accepted, thanks`))

	w := startWorker(t, pool, srv.URL+"/", defaults())
	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}

	if n := len(rec.all()); n != 1 {
		t.Fatalf("provider got %d POSTs, want 1", n)
	}
	if r.sentAt == nil || r.messageID != nil || r.timestamp != nil {
		t.Errorf("row = %+v, want sent_at set and both provider fields empty", r)
	}
	warnings := countLevel(w.logs.records(t), "WARN")
	if len(warnings) != 1 || warnings[0]["correlation_id"] != "corr-broken" || warnings[0]["notification_id"] != id {
		t.Errorf("warnings = %v, want one line with correlation_id corr-broken", warnings)
	}
}

// TestAcceptedBodyTimeout: the provider sends the 202 status line, flushes,
// and never finishes the body. The row becomes sent after the HTTP timeout,
// written with a new context inside the lease (K15).
func TestAcceptedBodyTimeout(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-stuck")[0]
	release := make(chan struct{})
	rec, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"messageId":"m-`)
		_ = http.NewResponseController(w).Flush() // httptest's writer supports Flush
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) }) // runs before srv.Close

	s := defaults()
	s.httpTimeout = 1 * time.Second
	w := startWorker(t, pool, srv.URL+"/", s)
	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}

	arrivals := rec.all()
	if len(arrivals) != 1 {
		t.Fatalf("provider got %d POSTs, want 1", len(arrivals))
	}
	if r.messageID != nil || r.timestamp != nil || r.sentAt == nil {
		t.Fatalf("row = %+v, want sent with empty provider fields", r)
	}
	// The write came after the HTTP timeout; the database clock and the test's
	// clock are the same machine's, so allow a small difference.
	if waited := r.sentAt.Sub(arrivals[0].at); waited < s.httpTimeout-50*time.Millisecond {
		t.Errorf("sent %s after the arrival, want at least the %s HTTP timeout", waited, s.httpTimeout)
	}
}

// TestNoRedirect: a 307 or 308 is not followed, so the redirect target gets
// no POST, and the notification is not sent (K14). The 3xx is a terminal
// protocol error, so the row fails on this attempt (K41).
func TestNoRedirect(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			pool := testdb.New(t)
			id := insertQueued(t, pool, 1, "corr-redirect")[0]
			target, targetSrv := newProvider(t, accepted(`{"messageId":"m-1","status":"accepted","timestamp":"2026-11-20T10:30:00Z"}`))
			first, firstSrv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, targetSrv.URL+"/", code)
			})

			w := startWorker(t, pool, firstSrv.URL+"/", defaults())
			r := waitStatus(t, pool, id, "failed", 10*time.Second)
			if err := w.stop(t); err != nil {
				t.Fatalf("worker: %v", err)
			}

			if n := len(first.all()); n != 1 {
				t.Errorf("first server got %d POSTs, want 1", n)
			}
			if n := len(target.all()); n != 0 {
				t.Errorf("redirect target got %d requests, want 0", n)
			}
			if r.sentAt != nil || r.lastError == nil || *r.lastError != "terminal: unexpected status "+strconv.Itoa(code) {
				t.Errorf("row = %+v, want not sent, last_error terminal: unexpected status %d", r, code)
			}
		})
	}
}

// TestWorkerShutdown: with one call slot, the stop arrives while the first
// call is in flight. No second claim follows; the call finishes and its row
// becomes sent; the other nine stay queued (K6).
func TestWorkerShutdown(t *testing.T) {
	pool := testdb.New(t)
	insertQueued(t, pool, 10, "corr-shutdown")
	arrived := make(chan struct{}, 10)
	var posts atomic.Int32
	_, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		arrived <- struct{}{}
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
			return
		}
		accepted(`{"messageId":"m-1","status":"accepted","timestamp":"2026-11-20T10:30:00Z"}`)(w, r)
	})

	s := defaults()
	s.cfg.InFlight = 1
	w := startWorker(t, pool, srv.URL+"/", s)
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("no call reached the provider")
	}
	stoppedAt := time.Now()
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}
	took := time.Since(stoppedAt)
	if took > 10*time.Second {
		t.Fatalf("worker returned %s after the stop, want within 10 s", took)
	}
	t.Logf("worker returned %s after the stop", took.Round(time.Millisecond))

	var sent, queued, claimed int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FILTER (WHERE status = 'sent'),
		        count(*) FILTER (WHERE status = 'queued' AND attempts = 0),
		        count(*) FILTER (WHERE attempts > 0)
		 FROM notifications`).Scan(&sent, &queued, &claimed); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || queued != 9 || claimed != 1 {
		t.Fatalf("sent=%d untouched queued=%d claimed=%d, want 1, 9 and 1", sent, queued, claimed)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("provider got %d POSTs, want 1", n)
	}
}
