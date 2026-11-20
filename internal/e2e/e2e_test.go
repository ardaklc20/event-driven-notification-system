package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/internal/worker"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m))
}

const smsBody = `{"recipient":"+905551234567","channel":"sms","content":"hello"}`

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

// providerStub records each request and hands it to answer.
type providerStub struct {
	mu       sync.Mutex
	requests []http.Header
	answer   http.HandlerFunc
}

func (p *providerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	p.mu.Lock()
	p.requests = append(p.requests, r.Header.Clone())
	p.mu.Unlock()
	p.answer(w, r)
}

func (p *providerStub) headers() []http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]http.Header(nil), p.requests...)
}

func accept(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"messageId":"m-e2e","status":"accepted","timestamp":"2026-11-20T10:30:00Z"}`)
}

// stack is the API, a worker and a provider stub on one test database.
type stack struct {
	pool        *pgxpool.Pool
	apiURL      string
	provider    *providerStub
	providerURL string
	apiLogs     *logBuffer
	workerLogs  *logBuffer
}

// newStack starts the API, the provider stub and the worker.
func newStack(t *testing.T, answer http.HandlerFunc) *stack {
	t.Helper()
	s := newPausedStack(t, answer)
	s.startWorker(t)
	return s
}

// newPausedStack starts the API and the provider stub; the worker starts
// only with startWorker.
func newPausedStack(t *testing.T, answer http.HandlerFunc) *stack {
	t.Helper()
	s := &stack{
		pool:       testdb.New(t),
		provider:   &providerStub{answer: answer},
		apiLogs:    &logBuffer{},
		workerLogs: &logBuffer{},
	}
	providerSrv := httptest.NewServer(s.provider)
	t.Cleanup(providerSrv.Close)

	apiSrv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Logger:        slog.New(slog.NewJSONHandler(s.apiLogs, nil)),
		Readiness:     &httpapi.Readiness{},
		Notifications: service.NewNotifications(postgres.NewStore(s.pool), domain.DefaultLimits()),
	}))
	t.Cleanup(apiSrv.Close)
	s.apiURL = apiSrv.URL
	s.providerURL = providerSrv.URL + "/"
	return s
}

// startWorker runs a worker against the stack until the end of the test.
func (s *stack) startWorker(t *testing.T) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(s.workerLogs, nil)).With(slog.String("worker_id", "e2e-worker"))
	w := worker.New(postgres.NewStore(s.pool), provider.NewClient(s.providerURL, 5*time.Second, 4), logger, worker.Config{
		Profile:          provider.ProfileStandard,
		LeaseDuration:    15 * time.Second,
		ResultBudget:     5 * time.Second,
		InFlight:         4,
		IdleWait:         20 * time.Millisecond,
		ShutdownBudget:   10 * time.Second,
		MaxAttempts:      5,
		BackoffBase:      0,
		BackoffCap:       0,
		RecoveryInterval: time.Second,
		RecoveryBatch:    100,
		PermitInterval:   11 * time.Millisecond,
		PermitGrace:      100 * time.Millisecond,
		Breaker:          domain.BreakerPolicy{Threshold: 5, Cooldown: 30 * time.Second, MaxCooldown: 5 * time.Minute},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("worker did not stop within 15 seconds")
		}
	})
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (s *stack) create(t *testing.T, body string, header http.Header) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.apiURL+"/notifications", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		req.Header[name] = values
	}
	return do(t, req)
}

func (s *stack) get(t *testing.T, id string) httpapi.NotificationBody {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.apiURL+"/notifications/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := do(t, req)
	if r.status != http.StatusOK {
		t.Fatalf("GET %s: status %d; body %s", id, r.status, r.body)
	}
	var n httpapi.NotificationBody
	if err := json.Unmarshal(r.body, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func do(t *testing.T, req *http.Request) reply {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: b}
}

func idOf(t *testing.T, r reply) string {
	t.Helper()
	if r.status != http.StatusAccepted {
		t.Fatalf("create: status %d, want 202; body %s", r.status, r.body)
	}
	var a httpapi.AcceptedBody
	if err := json.Unmarshal(r.body, &a); err != nil || a.ID == "" {
		t.Fatalf("create body %s: %v", r.body, err)
	}
	return a.ID
}

// waitSent polls GET until the notification is sent, for at most within.
func (s *stack) waitSent(t *testing.T, id string, within time.Duration) httpapi.NotificationBody {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := s.get(t, id)
		if n.Status == "sent" {
			return n
		}
		if time.Now().After(deadline) {
			t.Fatalf("notification %s is %s after %s, want sent", id, n.Status, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func correlation(id string) http.Header {
	return http.Header{httpapi.CorrelationIDHeader: {id}}
}

// TestCreateWhileProviderBlocked: while the provider holds A's request, B is
// still accepted at once, because the API never calls the provider (R7).
func TestCreateWhileProviderBlocked(t *testing.T) {
	arrived := make(chan struct{}, 10)
	release := make(chan struct{})
	var releaseOnce sync.Once
	s := newStack(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
			accept(w)
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	a := idOf(t, s.create(t, smsBody, correlation("blocked-a")))
	select {
	case <-arrived: // the worker really called the provider
	case <-time.After(5 * time.Second):
		t.Fatal("A's request never reached the provider")
	}

	start := time.Now()
	b := idOf(t, s.create(t, smsBody, correlation("blocked-b")))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("B took %s while the provider held A, want under 1 s", took)
	}
	if n := s.get(t, a); n.Status != "processing" {
		t.Fatalf("A is %s while the provider holds it, want processing", n.Status)
	}
	t.Logf("B accepted in %s while the provider held A", time.Since(start).Round(time.Millisecond))

	releaseOnce.Do(func() { close(release) })
	s.waitSent(t, a, 10*time.Second)
	s.waitSent(t, b, 10*time.Second)
}

// TestCorrelationIDEndToEnd: the client's X-Correlation-ID reaches the
// provider, and the API's and the worker's log lines for the notification
// carry it (R15, K21).
func TestCorrelationIDEndToEnd(t *testing.T) {
	s := newStack(t, func(w http.ResponseWriter, _ *http.Request) { accept(w) })

	id := idOf(t, s.create(t, smsBody, correlation("test-123")))
	s.waitSent(t, id, 10*time.Second)

	headers := s.provider.headers()
	if len(headers) != 1 || headers[0].Get(provider.CorrelationIDHeader) != "test-123" {
		t.Fatalf("provider got %d requests, header %v; want one with X-Correlation-ID test-123", len(headers), headers)
	}

	check := func(name string, logs *logBuffer, atLeast int) {
		t.Helper()
		var lines int
		for _, rec := range logs.records(t) {
			if rec["notification_id"] != id {
				continue
			}
			lines++
			if rec["correlation_id"] != "test-123" {
				t.Errorf("%s line %v has correlation_id %v, want test-123", name, rec["msg"], rec["correlation_id"])
			}
		}
		if lines < atLeast {
			t.Errorf("%s: %d lines for the notification, want at least %d", name, lines, atLeast)
		}
	}
	check("api", s.apiLogs, 1)       // notification accepted
	check("worker", s.workerLogs, 2) // start line and result line
}

// TestReplayAfterSent: a repeat with the same key after the notification was
// sent returns the first 202 body byte for byte, keeps the first correlation
// ID and starts no provider call (R11, K10, K21).
func TestReplayAfterSent(t *testing.T) {
	s := newStack(t, func(w http.ResponseWriter, _ *http.Request) { accept(w) })
	key := http.Header{httpapi.IdempotencyKeyHeader: {"k1"}}

	firstHeader := correlation("first")
	firstHeader[httpapi.IdempotencyKeyHeader] = key[httpapi.IdempotencyKeyHeader]
	first := s.create(t, smsBody, firstHeader)
	id := idOf(t, first)
	s.waitSent(t, id, 10*time.Second)

	secondHeader := correlation("second")
	secondHeader[httpapi.IdempotencyKeyHeader] = key[httpapi.IdempotencyKeyHeader]
	replay := s.create(t, smsBody, secondHeader)
	if replay.status != http.StatusAccepted || !bytes.Equal(replay.body, first.body) {
		t.Fatalf("replay: status %d body %s, want 202 and the first body %s", replay.status, replay.body, first.body)
	}
	if !bytes.Contains(replay.body, []byte(`"status":"queued"`)) {
		t.Errorf("replay body %s, want the first response's status queued", replay.body)
	}
	if n := s.get(t, id); n.Status != "sent" {
		t.Errorf("GET shows %s, want sent", n.Status)
	}

	var stored string
	if err := s.pool.QueryRow(t.Context(), `SELECT correlation_id FROM notifications WHERE id = $1::uuid`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "first" {
		t.Errorf("stored correlation ID %q, want first", stored)
	}

	// Give the worker several idle rounds to start a call if there were one.
	time.Sleep(300 * time.Millisecond)
	if n := len(s.provider.headers()); n != 1 {
		t.Fatalf("provider counted %d POSTs, want 1", n)
	}
	var rows int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM notifications`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d notifications (err %v), want 1", rows, err)
	}
}

// TestBurstAccepted: 3,000 creates, as fast as the test can send them, across
// the three channels. Every create is accepted at once and buffered in the
// queue; the worker then sends every one within its rate limit (N1).
func TestBurstAccepted(t *testing.T) {
	s := newStack(t, func(w http.ResponseWriter, _ *http.Request) { accept(w) })
	const perChannel = 1000
	bodies := map[string]string{
		"sms":   `{"recipient":"+905551234567","channel":"sms","content":"burst"}`,
		"email": `{"recipient":"user@example.com","channel":"email","content":"burst"}`,
		"push":  `{"recipient":"device-token-1","channel":"push","content":"burst"}`,
	}
	jobs := make(chan string)
	statuses := make(chan int, 3*perChannel)
	var wg sync.WaitGroup
	start := time.Now()
	for range 50 {
		wg.Go(func() {
			for body := range jobs {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.apiURL+"/notifications",
					strings.NewReader(body))
				if err != nil {
					statuses <- 0
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					statuses <- 0
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				statuses <- resp.StatusCode
			}
		})
	}
	for range perChannel {
		for _, ch := range []string{"sms", "email", "push"} {
			jobs <- bodies[ch]
		}
	}
	close(jobs)
	wg.Wait()
	close(statuses)
	accepted := time.Since(start)

	counts := map[int]int{}
	for code := range statuses {
		counts[code]++
	}
	if counts[http.StatusAccepted] != 3*perChannel {
		t.Fatalf("create statuses %v, want %d × 202", counts, 3*perChannel)
	}
	var rows int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM notifications`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3*perChannel {
		t.Fatalf("%d rows, want %d", rows, 3*perChannel)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		var sent int
		if err := s.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM notifications WHERE status = 'sent'`).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if sent == 3*perChannel {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d sent after 60 s", sent, 3*perChannel)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("3,000 creates accepted in %s; all sent %s after the first create",
		accepted.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
}

// cancel posts to /notifications/{id}/cancel.
func (s *stack) cancel(t *testing.T, id string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.apiURL+"/notifications/"+id+"/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, req)
}

// requestsFor counts the POSTs the provider received with correlation ID.
func (s *stack) requestsFor(correlationID string) int {
	n := 0
	for _, h := range s.provider.headers() {
		if h.Get(provider.CorrelationIDHeader) == correlationID {
			n++
		}
	}
	return n
}

// TestNoPostAfterCancel: with the worker not yet running, a notification is
// created and cancelled; the worker then runs for several claim cycles and
// never posts it, while a notification created after it is sent (R5, K9).
func TestNoPostAfterCancel(t *testing.T) {
	s := newPausedStack(t, func(w http.ResponseWriter, _ *http.Request) { accept(w) })
	id := idOf(t, s.create(t, smsBody, correlation("cancel-me")))
	if r := s.cancel(t, id); r.status != http.StatusOK {
		t.Fatalf("cancel: status %d, body %s; want 200", r.status, r.body)
	}
	s.startWorker(t)
	// A later notification on the same channel shows the worker has claimed
	// past the cancelled one.
	marker := idOf(t, s.create(t, smsBody, correlation("after-cancel")))
	s.waitSent(t, marker, 10*time.Second)
	time.Sleep(200 * time.Millisecond) // several 20 ms idle cycles more

	n := s.get(t, id)
	if n.Status != "cancelled" || n.Attempts != 0 || s.requestsFor("cancel-me") != 0 {
		t.Fatalf("cancelled notification: status %s, attempts %d, %d POSTs; want cancelled, 0, 0",
			n.Status, n.Attempts, s.requestsFor("cancel-me"))
	}
}

// TestCancelDuringRetryWait: the provider answers 503 with Retry-After: 2
// once; while the notification waits in queued for its retry, it is
// cancelled. After the retry time has passed, no second POST arrives (R5,
// K9).
func TestCancelDuringRetryWait(t *testing.T) {
	var calls atomic.Int32
	s := newStack(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		accept(w)
	})
	id := idOf(t, s.create(t, smsBody, correlation("retry-wait")))

	deadline := time.Now().Add(10 * time.Second)
	for {
		n := s.get(t, id)
		if n.Status == "queued" && n.Attempts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notification %s with %d attempts, want queued after the first 503", n.Status, n.Attempts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var due time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT available_at FROM notifications WHERE id = $1::uuid`, id).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if r := s.cancel(t, id); r.status != http.StatusOK {
		t.Fatalf("cancel during the retry wait: status %d, body %s; want 200", r.status, r.body)
	}
	// Wait until the retry time has passed by the database clock, then a few
	// idle cycles more.
	for {
		var passed bool
		if err := s.pool.QueryRow(t.Context(), `SELECT clock_timestamp() > $1`, due).Scan(&passed); err != nil {
			t.Fatal(err)
		}
		if passed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	n := s.get(t, id)
	if n.Status != "cancelled" || n.Attempts != 1 || s.requestsFor("retry-wait") != 1 {
		t.Fatalf("after the retry time: status %s, attempts %d, %d POSTs; want cancelled, 1, 1",
			n.Status, n.Attempts, s.requestsFor("retry-wait"))
	}
}
