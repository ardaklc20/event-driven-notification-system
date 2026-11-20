package worker_test

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

const validAccepted = `{"messageId":"m-1","status":"accepted","timestamp":"2026-11-20T10:30:00Z"}`

// sequence answers the i-th request with codes[i]; the last code repeats. A
// 202 carries a valid body.
func sequence(codes ...int) http.HandlerFunc {
	var (
		mu sync.Mutex
		i  int
	)
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		code := codes[min(i, len(codes)-1)]
		i++
		mu.Unlock()
		if code == http.StatusAccepted {
			accepted(validAccepted)(w, r)
			return
		}
		w.WriteHeader(code)
	}
}

// settle gives the worker several idle rounds to send a POST it should not.
func settle() { time.Sleep(300 * time.Millisecond) }

// attemptSequence runs one notification against a provider answering codes in
// order, waits for want and returns the row and the number of POSTs.
func attemptSequence(t *testing.T, s settings, want string, codes ...int) (row, int) {
	t.Helper()
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-retry")[0]
	rec, srv := newProvider(t, sequence(codes...))
	w := startWorker(t, pool, srv.URL+"/", s)
	r := waitStatus(t, pool, id, want, 10*time.Second)
	settle()
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}
	return r, len(rec.all())
}

// TestRetryThenSent: 503, then 202 (R12, K7).
func TestRetryThenSent(t *testing.T) {
	r, posts := attemptSequence(t, defaults(), "sent", 503, 202)
	if posts != 2 || r.attempts != 2 || r.sentAt == nil {
		t.Fatalf("%d POSTs, row %+v; want 2 POSTs, sent, attempts 2", posts, r)
	}
}

// TestSentOnFifthAttempt: four 503s, then 202 (K7).
func TestSentOnFifthAttempt(t *testing.T) {
	r, posts := attemptSequence(t, defaults(), "sent", 503, 503, 503, 503, 202)
	if posts != 5 || r.attempts != 5 {
		t.Fatalf("%d POSTs, row %+v; want 5 POSTs, sent, attempts 5", posts, r)
	}
}

// TestFailsAfterFiveTemporary: five 503s end as failed, with no sixth POST
// (K7).
func TestFailsAfterFiveTemporary(t *testing.T) {
	r, posts := attemptSequence(t, defaults(), "failed", 503)
	if posts != 5 || r.attempts != 5 || r.lastError == nil || *r.lastError != "temporary: status 503" {
		t.Fatalf("%d POSTs, row %+v; want 5 POSTs, failed, last_error naming 503", posts, r)
	}
}

// TestPermanentAfterTemporary: 503, then 400 fails on the second attempt
// with three attempts left (K7).
func TestPermanentAfterTemporary(t *testing.T) {
	r, posts := attemptSequence(t, defaults(), "failed", 503, 400)
	if posts != 2 || r.attempts != 2 || r.lastError == nil || *r.lastError != "permanent: status 400" {
		t.Fatalf("%d POSTs, row %+v; want 2 POSTs, failed after the second", posts, r)
	}
}

// TestPermanentErrorFailsOnce: a 400, and a 2xx other than 202 (K41), each
// fail after one POST.
func TestPermanentErrorFailsOnce(t *testing.T) {
	for _, tt := range []struct {
		code    int
		lastErr string
	}{
		{400, "permanent: status 400"},
		{200, "terminal: unexpected status 200"},
	} {
		t.Run(strconv.Itoa(tt.code), func(t *testing.T) {
			r, posts := attemptSequence(t, defaults(), "failed", tt.code)
			if posts != 1 || r.attempts != 1 || r.lastError == nil || *r.lastError != tt.lastErr {
				t.Fatalf("%d POSTs, row %+v; want 1 POST, failed, last_error %q", posts, r, tt.lastErr)
			}
		})
	}
}

// TestRetryAfterRespected: a 503 with Retry-After: 2. The second POST arrives
// no earlier than 2 seconds after the first response, measured at the
// provider, and available_at is 2 seconds after the retry write (K7).
func TestRetryAfterRespected(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-retry-after")[0]
	var (
		mu            sync.Mutex
		firstAnswered time.Time
		calls         int
	)
	rec, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if !first {
			accepted(validAccepted)(w, r)
			return
		}
		w.Header().Set("Retry-After", "2")
		mu.Lock()
		firstAnswered = time.Now()
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	w := startWorker(t, pool, srv.URL+"/", defaults()) // zero backoff: only Retry-After delays
	// The retry write: back in queued after the first attempt, with its error.
	waitRow(t, pool, id, 5*time.Second, "queued after the first 503", func(r row) bool {
		return r.status == "queued" && r.attempts == 1 && r.lastError != nil
	})
	var gap time.Duration
	if err := pool.QueryRow(t.Context(),
		`SELECT available_at - updated_at FROM notifications WHERE id = $1::uuid`, id).Scan(&gap); err != nil {
		t.Fatal(err)
	}
	if gap < 2*time.Second {
		t.Errorf("available_at is %s after the retry write, want at least 2s", gap)
	}

	waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}
	arrivals := rec.all()
	if len(arrivals) != 2 {
		t.Fatalf("%d POSTs, want 2", len(arrivals))
	}
	mu.Lock()
	waited := arrivals[1].at.Sub(firstAnswered)
	mu.Unlock()
	t.Logf("second POST arrived %s after the first response", waited.Round(time.Millisecond))
	if waited < 2*time.Second {
		t.Fatalf("second POST arrived %s after the first response, want at least 2s", waited)
	}
}

// TestTooManyRequestsByProfile: the same 429 stops at once under the free
// webhook.site profile and is retried under the standard one (K7, K14).
func TestTooManyRequestsByProfile(t *testing.T) {
	t.Run(string(provider.ProfileWebhookSiteFree), func(t *testing.T) {
		s := defaults()
		s.cfg.Profile = provider.ProfileWebhookSiteFree
		r, posts := attemptSequence(t, s, "failed", 429)
		if posts != 1 || r.lastError == nil || *r.lastError != "permanent: status 429 (webhook.site quota used up)" {
			t.Fatalf("%d POSTs, row %+v; want 1 POST and failed", posts, r)
		}
	})
	t.Run(string(provider.ProfileStandard), func(t *testing.T) {
		r, posts := attemptSequence(t, defaults(), "sent", 429, 202)
		if posts != 2 || r.attempts != 2 {
			t.Fatalf("%d POSTs, row %+v; want the 429 retried, then sent", posts, r)
		}
	})
}
