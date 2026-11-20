package worker_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/internal/worker"
)

// gate holds stopped deliveries until it is opened; opening twice is safe.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate  { return &gate{ch: make(chan struct{})} }
func (g *gate) wait() { <-g.ch }
func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }
func (g *gate) openAtEnd(t *testing.T) {
	// Call it after startWorker: cleanups run last-in first-out, so the
	// stopped deliveries are released before the worker is asked to stop.
	t.Cleanup(g.open)
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never happened", what)
		var zero T
		return zero
	}
}

// TestLeaseRecovery: worker A claims and stops before its POST, as if it had
// crashed. B's recovery returns the row to queued; B claims it with a new
// token and sends it. A, released afterwards, finds too little lease left and
// sends nothing (N3, K3, K9).
func TestLeaseRecovery(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-recovery")[0]
	rec, srv := newProvider(t, accepted(validAccepted))

	stopA := newGate()
	claimedA := make(chan domain.Claim, 1)
	a := defaults()
	a.cfg.LeaseDuration = time.Second
	a.cfg.ResultBudget = 500 * time.Millisecond
	a.cfg.InFlight = 1 // the stopped delivery holds A's only slot
	a.cfg.RecoveryInterval = time.Hour
	a.hooks.AfterClaim = func(c domain.Claim) {
		claimedA <- c
		stopA.wait()
	}
	wa := startWorker(t, pool, srv.URL+"/", a)
	stopA.openAtEnd(t)
	ca := receive(t, claimedA, "A's claim")

	claimedB := make(chan domain.Claim, 5)
	b := defaults()
	b.hooks.AfterClaim = func(c domain.Claim) { claimedB <- c }
	wb := startWorker(t, pool, srv.URL+"/", b)

	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	cb := receive(t, claimedB, "B's claim")
	if cb.ID != id || cb.LeaseToken == ca.LeaseToken || cb.Attempt != 2 || r.attempts != 2 {
		t.Fatalf("A's claim %+v, B's claim %+v, row %+v; want B on the same row with a new token, attempt 2", ca, cb, r)
	}

	stopA.open()
	for name, w := range map[string]*running{"A": wa, "B": wb} {
		if err := w.stop(t); err != nil {
			t.Fatalf("worker %s: %v", name, err)
		}
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("provider got %d POSTs, want only B's", n)
	}
	if !hasMessage(wa.logs.records(t), "lease too short to send") {
		t.Error("A logged no skipped call after its release")
	}
}

func hasMessage(records []map[string]any, part string) bool {
	return slices.ContainsFunc(records, func(r map[string]any) bool {
		msg, _ := r["msg"].(string)
		return strings.Contains(msg, part)
	})
}

// countingStore counts recovery statements.
type countingStore struct {
	worker.Store
	recoveries atomic.Int32
}

func (c *countingStore) RecoverExpired(ctx context.Context, maxAttempts, batch int) ([]domain.Recovered, error) {
	c.recoveries.Add(1)
	return c.Store.RecoverExpired(ctx, maxAttempts, batch)
}

// TestRecoveryLoop: a running worker recovers an expired lease on its own
// within 2 seconds, and its recovery loop ends with the worker (K40).
func TestRecoveryLoop(t *testing.T) {
	pool := testdb.New(t)
	_, srv := newProvider(t, accepted(validAccepted))
	// Due in an hour once recovered, so the worker does not claim it again
	// before the test sees it queued.
	var id string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status, attempts,
		                            lease_token, lease_expires_at, available_at)
		 VALUES ('+905551234567', 'sms', 'hello', 'normal', 'corr-loop', 'processing', 1,
		         gen_random_uuid(), clock_timestamp() - interval '1 second', now() + interval '1 hour')
		 RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	counter := &countingStore{}
	s := defaults()
	s.cfg.RecoveryInterval = 100 * time.Millisecond
	s.wrap = func(st worker.Store) worker.Store {
		counter.Store = st
		return counter
	}
	w := startWorker(t, pool, srv.URL+"/", s)
	r := waitStatus(t, pool, id, "queued", 2*time.Second)
	if r.token != nil || r.expires != nil || r.attempts != 1 || r.lastError == nil || *r.lastError != postgres.RecoveredError {
		t.Fatalf("row = %+v, want queued, no lease, attempts 1, last_error about the expired lease", r)
	}

	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}
	after := counter.recoveries.Load()
	time.Sleep(5 * s.cfg.RecoveryInterval)
	if n := counter.recoveries.Load(); n != after || after == 0 {
		t.Fatalf("%d recovery statements before the stop returned, %d after; want > 0 and no more", after, n)
	}
}

// TestLate202AfterFailed: on the fifth attempt the provider answers 202 at
// once, but the worker stops right before its result write. The lease
// expires, recovery fails the row, then the worker is released: its late
// result is not recorded (K3, K9, K15).
func TestLate202AfterFailed(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-late")[0]
	if _, err := pool.Exec(t.Context(), `UPDATE notifications SET attempts = 4`); err != nil {
		t.Fatal(err)
	}
	rec, srv := newProvider(t, accepted(validAccepted))

	hold := newGate()
	atWrite := make(chan domain.Claim, 1)
	s := defaults()
	s.cfg.LeaseDuration = time.Second
	s.cfg.ResultBudget = 300 * time.Millisecond
	s.cfg.RecoveryInterval = time.Hour // the test runs recovery itself
	s.hooks.BeforeWrite = func(c domain.Claim) {
		atWrite <- c
		hold.wait()
	}
	w := startWorker(t, pool, srv.URL+"/", s)
	hold.openAtEnd(t)
	c := receive(t, atWrite, "the fifth attempt's result write")
	if c.Attempt != 5 {
		t.Fatalf("attempt %d, want 5", c.Attempt)
	}

	waitRow(t, pool, id, 5*time.Second, "an expired lease", func(row) bool {
		var expired bool
		_ = pool.QueryRow(context.Background(),
			`SELECT lease_expires_at <= clock_timestamp() FROM notifications WHERE id = $1::uuid`, id).Scan(&expired)
		return expired
	})
	got, err := postgres.NewStore(pool).RecoverExpired(t.Context(), s.cfg.MaxAttempts, s.cfg.RecoveryBatch)
	if err != nil || len(got) != 1 || got[0].Status != domain.StatusFailed {
		t.Fatalf("recovery: %+v, err %v; want the row failed", got, err)
	}

	hold.open()
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}
	r := readRow(t, pool, id)
	if r.status != "failed" || r.sentAt != nil || r.messageID != nil || r.timestamp != nil {
		t.Fatalf("row = %+v, want failed with no sent_at and no provider fields", r)
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("provider got %d POSTs, want 1", n)
	}
}

// TestUnclearOutcomeRetries: the provider takes the first request but answers
// only after the worker's timeout; the second it answers at once. The worker
// cannot know the first was received, so it retries: two calls for the same
// notification, final sent. This is where the guarantee ends (R11, K3 test 3).
func TestUnclearOutcomeRetries(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-unclear")[0]
	var calls atomic.Int32
	rec, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done() // answers only after the worker gave up
		}
		accepted(validAccepted)(w, r)
	})

	s := defaults()
	s.httpTimeout = 500 * time.Millisecond
	w := startWorker(t, pool, srv.URL+"/", s)
	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	settle()
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}

	arrivals := rec.all()
	if len(arrivals) != 2 {
		t.Fatalf("provider got %d calls, want 2", len(arrivals))
	}
	for i, a := range arrivals {
		if got := a.header.Get("X-Correlation-ID"); got != "corr-unclear" {
			t.Errorf("call %d carried correlation ID %q, want the notification's", i+1, got)
		}
	}
	if r.attempts != 2 || r.lastError == nil || *r.lastError != "temporary: provider call: context deadline exceeded" {
		t.Fatalf("row = %+v, want sent on attempt 2 after a timeout", r)
	}
}

// TestAttemptCountedOnCrash: every attempt stops right after the provider has
// received its POST, before the result is written. Each attempt is counted at
// its claim; after the fifth lease expires the row is failed and the provider
// never receives a sixth POST (K9).
func TestAttemptCountedOnCrash(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-crash")[0]
	rec, srv := newProvider(t, accepted(validAccepted))

	hold := newGate()
	var (
		mu       sync.Mutex
		attempts []int
	)
	s := defaults()
	s.cfg.LeaseDuration = 600 * time.Millisecond
	s.cfg.ResultBudget = 200 * time.Millisecond
	s.cfg.InFlight = 10 // each stopped delivery keeps its slot
	s.cfg.RecoveryInterval = 50 * time.Millisecond
	s.hooks.AfterClaim = func(c domain.Claim) {
		mu.Lock()
		attempts = append(attempts, c.Attempt)
		mu.Unlock()
	}
	s.hooks.BeforeWrite = func(domain.Claim) { hold.wait() }
	w := startWorker(t, pool, srv.URL+"/", s)
	hold.openAtEnd(t)

	r := waitStatus(t, pool, id, "failed", 15*time.Second)
	time.Sleep(time.Second) // a sixth POST would arrive here
	posts := len(rec.all())
	hold.open()
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(attempts, []int{1, 2, 3, 4, 5}) {
		t.Errorf("claims counted attempts %v, want 1 to 5", attempts)
	}
	if posts != 5 || r.attempts != 5 || r.lastError == nil || *r.lastError != postgres.RecoveredError {
		t.Fatalf("%d POSTs, row %+v; want 5 POSTs and failed by recovery after attempt 5", posts, r)
	}
}

// TestSendDeadlineBeforeLease: the lease left after the claim is shorter than
// the result budget, so the worker sends nothing (K9).
func TestSendDeadlineBeforeLease(t *testing.T) {
	pool := testdb.New(t)
	insertQueued(t, pool, 1, "corr-short")
	rec, srv := newProvider(t, accepted(validAccepted))

	s := defaults()
	s.cfg.LeaseDuration = time.Second
	s.cfg.ResultBudget = 2 * time.Second
	w := startWorker(t, pool, srv.URL+"/", s)

	deadline := time.Now().Add(5 * time.Second)
	for !hasMessage(w.logs.records(t), "lease too short to send") {
		if time.Now().After(deadline) {
			t.Fatal("the worker never skipped a call")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond) // through a recovery and a second claim
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if n := len(rec.all()); n != 0 {
		t.Fatalf("provider got %d POSTs, want none", n)
	}
}
