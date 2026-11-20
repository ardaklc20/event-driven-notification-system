package worker_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
	"github.com/ardaklc20/event-driven-notification-system/internal/ratecheck"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/internal/worker"
)

const (
	startLine  = "provider call started"
	missedLine = "start deadline missed"
)

// insertOn inserts n queued notifications on ch with correlation IDs
// <ch>-1 … <ch>-n.
func insertOn(t *testing.T, pool *pgxpool.Pool, ch domain.Channel, n int) {
	t.Helper()
	recipient := map[domain.Channel]string{
		domain.ChannelSMS: "+905551234567", domain.ChannelEmail: "user@example.com", domain.ChannelPush: "device-token-1",
	}[ch]
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
		 SELECT $1, $2, 'hello', 'normal', $2 || '-' || g FROM generate_series(1, $3) AS g`,
		recipient, string(ch), n); err != nil {
		t.Fatal(err)
	}
}

// secondPool opens another pool on the test database, as a second worker
// process would.
func secondPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	other, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return other
}

// starts reads the send starts from the workers' start lines (K21, K42).
func starts(t *testing.T, workers ...*running) []ratecheck.Start {
	t.Helper()
	var out []ratecheck.Start
	for _, w := range workers {
		for _, rec := range w.logs.records(t) {
			if rec["msg"] != startLine {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, rec["started_at"].(string))
			if err != nil {
				t.Fatalf("start line %v: %v", rec, err)
			}
			out = append(out, ratecheck.Start{
				Channel: rec["channel"].(string), WorkerID: rec["worker_id"].(string), At: at,
			})
		}
	}
	return out
}

// countMsg counts log lines whose message starts with prefix.
func countMsg(records []map[string]any, prefix string) int {
	n := 0
	for _, r := range records {
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, prefix) {
			n++
		}
	}
	return n
}

func readNextPermit(t *testing.T, pool *pgxpool.Pool, ch domain.Channel) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT next_permit_at FROM channel_limits WHERE channel = $1`, string(ch)).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// giveBackSpy reads the row and the permit right after a give-back.
type giveBackSpy struct {
	worker.Store
	t     *testing.T
	pool  *pgxpool.Pool
	after chan givenBack
}

type givenBack struct {
	given  bool
	row    row
	permit time.Time
}

func (g *giveBackSpy) GiveBack(ctx context.Context, id, leaseToken string) (bool, error) {
	given, err := g.Store.GiveBack(ctx, id, leaseToken)
	g.after <- givenBack{given: given, row: readRow(g.t, g.pool, id), permit: readNextPermit(g.t, g.pool, domain.ChannelSMS)}
	return given, err
}

// TestExpiredPermitNoPost: the worker stops right before the start gate and
// is released 200 ms later, after the 100 ms start deadline. No request
// reaches the provider and no start line is written; the claim is given back
// with its attempt, the permit stays taken, and a later claim sends the
// notification once (K42).
func TestExpiredPermitNoPost(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-late")[0]
	rec, srv := newProvider(t, accepted(validAccepted))

	s := defaults()
	// One call slot: the worker cannot claim again until the give-back has
	// returned, so the spy sees the row as the give-back left it.
	s.cfg.InFlight = 1
	var (
		once        sync.Once
		permitAfter time.Time
	)
	s.hooks.AfterClaim = func(domain.Claim) {
		once.Do(func() { permitAfter = readNextPermit(t, pool, domain.ChannelSMS) })
	}
	var paused atomic.Bool
	s.hooks.BeforeGate = func(domain.Claim) {
		if paused.CompareAndSwap(false, true) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	spy := &giveBackSpy{t: t, pool: pool, after: make(chan givenBack, 1)}
	s.wrap = func(st worker.Store) worker.Store { spy.Store = st; return spy }
	w := startWorker(t, pool, srv.URL+"/", s)

	g := receive(t, spy.after, "the give-back")
	if !g.given || g.row.status != "queued" || g.row.attempts != 0 || g.row.token != nil || g.row.expires != nil {
		t.Fatalf("after the give-back: %+v, want queued, attempts 0, lease cleared", g)
	}
	if !g.permit.Equal(permitAfter) {
		t.Fatalf("next_permit_at %s after the give-back, %s after the claim: moved back", g.permit, permitAfter)
	}
	r := waitStatus(t, pool, id, "sent", 5*time.Second)
	settle()
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}

	logs := w.logs.records(t)
	if n := len(rec.all()); n != 1 || r.attempts != 1 {
		t.Fatalf("%d POSTs, row %+v; want one POST, sent on attempt 1", n, r)
	}
	if missed, started := countMsg(logs, missedLine), countMsg(logs, startLine); missed != 1 || started != 1 {
		t.Fatalf("%d %q lines and %d start lines, want 1 and 1 (the later send)", missed, missedLine, started)
	}
}

// TestSendStartsWithPausedWorker: worker A pauses 300 ms right after its
// first claim, while worker B keeps sending on a full channel. A's claim
// misses its start deadline and sends nothing; that notification is sent
// later, once; both workers start sends, and no half-open one-second window
// holds more than 100 starts (R8, N2, K42).
func TestSendStartsWithPausedWorker(t *testing.T) {
	pool := testdb.New(t)
	const total = 300
	insertOn(t, pool, domain.ChannelSMS, total)
	rec, srv := newProvider(t, accepted(validAccepted))

	a := defaults()
	a.id = "worker-a"
	pausedID := make(chan string, 1)
	var pausedOnce atomic.Bool
	a.hooks.AfterClaim = func(c domain.Claim) {
		if pausedOnce.CompareAndSwap(false, true) {
			pausedID <- c.ID
			time.Sleep(300 * time.Millisecond)
		}
	}
	b := defaults()
	b.id = "worker-b"
	wa := startWorker(t, pool, srv.URL+"/", a)
	wb := startWorker(t, secondPool(t, pool), srv.URL+"/", b)

	paused := receive(t, pausedID, "A's paused claim")
	waitCount(t, pool, "sent", total, 20*time.Second)
	settle()
	for _, w := range []*running{wa, wb} {
		if err := w.stop(t); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}

	all := starts(t, wa, wb)
	report := ratecheck.PerChannel(all)
	t.Logf("starts %+v, workers %v, arrivals %d", report, ratecheck.Workers(all), len(rec.all()))
	if len(report) != 1 || report[0].Starts != total || report[0].MaxWindow > 100 {
		t.Fatalf("starts %+v, want %d sms starts with at most 100 in any window", report, total)
	}
	if workers := ratecheck.Workers(all); len(workers) != 2 {
		t.Fatalf("starts from %v, want both workers", workers)
	}
	if n := countMsg(wa.logs.records(t), missedLine); n != 1 {
		t.Fatalf("worker A wrote %d %q lines, want 1", n, missedLine)
	}
	r := readRow(t, pool, paused)
	if r.status != "sent" || r.attempts != 1 {
		t.Fatalf("A's paused notification %+v, want sent once on attempt 1", r)
	}
	if n := len(rec.all()); n != total {
		t.Fatalf("provider got %d POSTs, want %d: one per notification", n, total)
	}
}

// waitCount polls until n notifications have status, for at most within.
func waitCount(t *testing.T, pool *pgxpool.Pool, status string, n int, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		var got int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM notifications WHERE status = $1::notification_status`, status).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == n {
			return time.Since(start)
		}
		if time.Since(start) > within {
			t.Fatalf("%d of %d notifications %s after %s", got, n, status, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// outcomes counts claim outcomes across workers.
type outcomes struct {
	mu     sync.Mutex
	counts map[domain.ClaimOutcome]int
}

func (o *outcomes) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return fmt.Sprint(o.counts)
}

// countingClaims counts the outcomes of its store's claims.
type countingClaims struct {
	worker.Store
	o *outcomes
}

func (c countingClaims) ClaimNext(ctx context.Context, req domain.ClaimRequest) (domain.ClaimResult, error) {
	res, err := c.Store.ClaimNext(ctx, req)
	if err == nil {
		c.o.mu.Lock()
		c.o.counts[res.Outcome]++
		c.o.mu.Unlock()
	}
	return res, err
}

// envInt and envDuration read the optional overrides of the in-flight
// measurement (slice 5, step 9).
func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

func envDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

// TestRateLimitTwoWorkers: two workers on separate pools, 1,000 queued per
// channel, and a provider that answers 503 to the first attempt of every
// tenth notification. Every notification ends sent within 30 seconds; retries
// are among the starts; both workers start sends; no channel has more than
// 100 starts in any window, while the three channels together exceed 100 in
// the busiest second (R8, N2, K5, K42).
//
// R8_IN_FLIGHT and R8_PROVIDER_DELAY override the calls in flight per worker
// and the provider's answer time, for the in-flight measurement.
func TestRateLimitTwoWorkers(t *testing.T) {
	pool := testdb.New(t)
	const perChannel = 1000
	for _, ch := range domain.Channels {
		insertOn(t, pool, ch, perChannel)
	}
	delay := envDuration(t, "R8_PROVIDER_DELAY", 0)
	var (
		mu    sync.Mutex
		tried = map[string]bool{}
	)
	rec, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		corr := r.Header.Get(provider.CorrelationIDHeader)
		n, _ := strconv.Atoi(corr[strings.LastIndex(corr, "-")+1:])
		mu.Lock()
		first := !tried[corr]
		tried[corr] = true
		mu.Unlock()
		if n%10 == 0 && first {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		accepted(validAccepted)(w, r)
	})

	var workers []*running
	inFlight := envInt(t, "R8_IN_FLIGHT", defaults().cfg.InFlight)
	claims := &outcomes{counts: map[domain.ClaimOutcome]int{}}
	for i, p := range []*pgxpool.Pool{pool, secondPool(t, pool)} {
		s := defaults()
		s.id = "worker-" + strconv.Itoa(i+1)
		s.cfg.InFlight = inFlight
		s.wrap = func(st worker.Store) worker.Store { return countingClaims{Store: st, o: claims} }
		s.httpTimeout = max(s.httpTimeout, 2*delay)
		workers = append(workers, startWorker(t, p, srv.URL+"/", s))
	}
	took := waitCount(t, pool, "sent", 3*perChannel, 30*time.Second)
	settle()
	for _, w := range workers {
		if err := w.stop(t); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}

	all := starts(t, workers...)
	report := ratecheck.PerChannel(all)
	combined := make([]time.Time, len(all))
	for i, s := range all {
		combined[i] = s.At
	}
	var retries int
	for _, w := range workers {
		for _, rec := range w.logs.records(t) {
			if rec["msg"] == startLine && rec["attempt"] == float64(2) {
				retries++
			}
		}
	}
	t.Logf("in_flight=%d provider_delay=%s: all sent after %s; starts %+v; three channels together %d in the busiest second; "+
		"retry starts %d; workers %v; arrivals %d; claim outcomes %s",
		inFlight, delay, took.Round(time.Millisecond), report,
		ratecheck.MaxInWindow(combined, ratecheck.Window), retries, ratecheck.Workers(all), len(rec.all()), claims)

	if len(report) != 3 {
		t.Fatalf("starts on %d channels, want 3", len(report))
	}
	for _, r := range report {
		if r.Starts != perChannel+perChannel/10 || r.MaxWindow > 100 {
			t.Errorf("%s: %d starts, at most %d in a window; want %d starts, at most 100", r.Channel, r.Starts,
				r.MaxWindow, perChannel+perChannel/10)
		}
	}
	if retries != 3*perChannel/10 {
		t.Errorf("%d retry starts, want %d", retries, 3*perChannel/10)
	}
	if w := ratecheck.Workers(all); len(w) != 2 {
		t.Errorf("starts from %v, want both workers", w)
	}
	if n := ratecheck.MaxInWindow(combined, ratecheck.Window); n <= 100 {
		t.Errorf("three channels together: %d starts in the busiest second, want more than 100", n)
	}
}
