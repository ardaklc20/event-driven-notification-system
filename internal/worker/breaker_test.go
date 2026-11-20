package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/internal/worker"
)

// The breaker scenarios (K34) run with two workers, each on its own pool as
// two processes would, against an httptest provider the test controls, with
// threshold 3 and cooldown 1 second.

const (
	openedLine   = "breaker opened"
	reopenedLine = "breaker reopened"
	closedLine   = "breaker closed"
)

func breakerSettings(id string) settings {
	s := defaults()
	s.id = id
	s.cfg.Breaker = domain.BreakerPolicy{Threshold: 3, Cooldown: time.Second, MaxCooldown: 4 * time.Second}
	return s
}

// pair starts worker A on pool and worker B on a second pool.
func pair(t *testing.T, pool *pgxpool.Pool, url string, a, b settings) (*running, *running) {
	t.Helper()
	return startWorker(t, pool, url, a), startWorker(t, secondPool(t, pool), url, b)
}

// breakerState is the provider_breaker row; openFor is the time left until
// open_until by the database clock, cooldown the current cooldown.
type breakerState struct {
	generation int64
	failures   int
	open       bool
	openFor    time.Duration
	cooldown   time.Duration
	probeToken string
}

func readBreaker(t *testing.T, pool *pgxpool.Pool) breakerState {
	t.Helper()
	var (
		b              breakerState
		openUS, coolUS int64
	)
	err := pool.QueryRow(context.Background(),
		`SELECT generation, consecutive_failures, open_until IS NOT NULL,
		        coalesce(floor(extract(epoch FROM open_until - clock_timestamp()) * 1000000)::bigint, 0),
		        floor(extract(epoch FROM cooldown) * 1000000)::bigint,
		        coalesce(probe_token::text, '')
		 FROM provider_breaker`).Scan(&b.generation, &b.failures, &b.open, &openUS, &coolUS, &b.probeToken)
	if err != nil {
		t.Fatalf("read breaker: %v", err)
	}
	b.openFor = time.Duration(openUS) * time.Microsecond
	b.cooldown = time.Duration(coolUS) * time.Microsecond
	return b
}

// sameBreaker compares two readings without the time left, which moves.
func sameBreaker(a, b breakerState) bool {
	a.openFor, b.openFor = 0, 0
	return a == b
}

func waitBreaker(t *testing.T, pool *pgxpool.Pool, within time.Duration, what string, ok func(breakerState) bool) breakerState {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		b := readBreaker(t, pool)
		if ok(b) {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("breaker after %s: %+v, want %s", within, b, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openAfterCooldown opens the breaker in generation 1 with a 2-second
// cooldown that is already over, so the next claim becomes the probe. The
// cooldown differs from the 1-second setting, so a close (back to 1 s) and a
// reopening (4 s) both show.
func openAfterCooldown(t *testing.T, pool *pgxpool.Pool) breakerState {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE provider_breaker
		 SET generation = 1, consecutive_failures = 0, cooldown = interval '2 seconds',
		     open_until = clock_timestamp() - interval '1 millisecond'`); err != nil {
		t.Fatal(err)
	}
	return readBreaker(t, pool)
}

// expireProbeLease ends the probe's lease by the database clock: the
// notification's lease and the probe slot's end. The worker's own deadline is
// still ahead, so its result write reaches the database, whose checks must
// refuse it (K3, K34).
func expireProbeLease(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE notifications SET lease_expires_at = clock_timestamp() - interval '1 millisecond' WHERE id = $1::uuid`,
		id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE provider_breaker SET probe_expires_at = clock_timestamp() - interval '1 millisecond'
		 WHERE probe_token IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
}

func statusCounts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status::text, count(*) FROM notifications GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			s string
			n int
		)
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func sumAttempts(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT coalesce(sum(attempts), 0) FROM notifications`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitAll polls until every notification has status want.
func waitAll(t *testing.T, pool *pgxpool.Pool, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		c := statusCounts(t, pool)
		if len(c) == 1 && c[want] > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statuses after %s: %v, want all %s", within, c, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lines returns the log records with message msg from every worker.
func lines(t *testing.T, msg string, workers ...*running) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, w := range workers {
		for _, rec := range w.logs.records(t) {
			if rec["msg"] == msg {
				out = append(out, rec)
			}
		}
	}
	return out
}

// switchable answers with the handler last set.
type switchable struct {
	h atomic.Pointer[http.HandlerFunc]
}

func newSwitchable(h http.HandlerFunc) *switchable {
	s := &switchable{}
	s.set(h)
	return s
}

func (s *switchable) set(h http.HandlerFunc) { s.h.Store(&h) }

func (s *switchable) serve(w http.ResponseWriter, r *http.Request) { (*s.h.Load())(w, r) }

// status answers with code and no body; 202 gets a valid body.
func status(code int) http.HandlerFunc {
	if code == http.StatusAccepted {
		return accepted(validAccepted)
	}
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func tooMany(retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
	}
}

// content reads the notification's content from a request body.
func content(r *http.Request) string {
	var body struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Content
}

// insertContent inserts one queued notification on ch with content and
// returns its ID.
func insertContent(t *testing.T, pool *pgxpool.Pool, ch domain.Channel, text string) string {
	t.Helper()
	recipient := map[domain.Channel]string{
		domain.ChannelSMS: "+905551234567", domain.ChannelEmail: "user@example.com", domain.ChannelPush: "device-token-1",
	}[ch]
	var id string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
		 VALUES ($1, $2, $3, 'normal', $3) RETURNING id::text`, recipient, string(ch), text).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// claimLog records the claims of every worker it is installed on.
type claimLog struct {
	mu     sync.Mutex
	claims []claimBy
	notify chan claimBy
}

type claimBy struct {
	worker string
	claim  domain.Claim
	at     time.Time
}

func newClaimLog() *claimLog { return &claimLog{notify: make(chan claimBy, 100)} }

func (l *claimLog) hook(id string) func(domain.Claim) {
	return func(c domain.Claim) {
		cb := claimBy{worker: id, claim: c, at: time.Now()}
		l.mu.Lock()
		l.claims = append(l.claims, cb)
		l.mu.Unlock()
		l.notify <- cb
	}
}

func (l *claimLog) probes() []claimBy {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []claimBy
	for _, c := range l.claims {
		if c.claim.Probe {
			out = append(out, c)
		}
	}
	return out
}

// finishSpy records every result write and its outcome; after, when set,
// runs once the write has returned.
type finishSpy struct {
	worker.Store
	sink  *finishLog
	after func(domain.Finish, domain.Finished)
}

type finishLog struct {
	mu    sync.Mutex
	calls []finishCall
}

type finishCall struct {
	f   domain.Finish
	fin domain.Finished
	err error
}

func (s *finishSpy) Finish(ctx context.Context, f domain.Finish) (domain.Finished, error) {
	fin, err := s.Store.Finish(ctx, f)
	s.sink.mu.Lock()
	s.sink.calls = append(s.sink.calls, finishCall{f: f, fin: fin, err: err})
	s.sink.mu.Unlock()
	if s.after != nil {
		s.after(f, fin)
	}
	return fin, err
}

func (l *finishLog) of(id string) []finishCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []finishCall
	for _, c := range l.calls {
		if c.f.Attempt.ID == id {
			out = append(out, c)
		}
	}
	return out
}

func spyOn(sink *finishLog, after func(domain.Finish, domain.Finished)) func(worker.Store) worker.Store {
	return func(st worker.Store) worker.Store { return &finishSpy{Store: st, sink: sink, after: after} }
}

// noRecovery skips lease recovery, which otherwise also runs once when a
// worker starts: a row whose lease the test ended stays as the worker left it.
type noRecovery struct {
	worker.Store
}

func (noRecovery) RecoverExpired(context.Context, int, int) ([]domain.Recovered, error) {
	return nil, nil
}

// spyWithoutRecovery is spyOn with lease recovery skipped.
func spyWithoutRecovery(sink *finishLog) func(worker.Store) worker.Store {
	return func(st worker.Store) worker.Store { return &finishSpy{Store: noRecovery{st}, sink: sink} }
}

// TestBreakerOpens: the provider answers 503. After three failures the
// breaker opens; then, until the cooldown ends, no worker claims on any
// channel and the waiting rows' attempts do not move (K34).
func TestBreakerOpens(t *testing.T) {
	pool := testdb.New(t)
	for _, ch := range domain.Channels {
		insertOn(t, pool, ch, 10)
	}
	rec, srv := newProvider(t, status(http.StatusServiceUnavailable))
	a, b := pair(t, pool, srv.URL+"/", breakerSettings("worker-a"), breakerSettings("worker-b"))

	opened := waitBreaker(t, pool, 10*time.Second, "open", func(b breakerState) bool { return b.open })
	// Calls that started before the opening finish and write their result.
	deadline := time.Now().Add(2 * time.Second)
	for statusCounts(t, pool)["processing"] > 0 {
		if time.Now().After(deadline) {
			t.Fatal("calls from before the opening still processing after 2 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	now := readBreaker(t, pool)
	if now.generation != opened.generation || now.openFor < 300*time.Millisecond {
		t.Fatalf("breaker %+v: want the same opening with at least 300 ms left", now)
	}
	posts, attempts := len(rec.all()), sumAttempts(t, pool)
	time.Sleep(now.openFor - 150*time.Millisecond)

	end := readBreaker(t, pool)
	if end.generation != opened.generation || !end.open || end.probeToken != "" {
		t.Fatalf("breaker %+v: want still open in generation %d with no probe", end, opened.generation)
	}
	if n := len(rec.all()); n != posts {
		t.Errorf("provider got %d POSTs while open, want none", n-posts)
	}
	if n := sumAttempts(t, pool); n != attempts {
		t.Errorf("attempts moved from %d to %d while open", attempts, n)
	}
	if c := statusCounts(t, pool); c["queued"] != 30 {
		t.Errorf("statuses %v while open, want all 30 queued", c)
	}
	if posts < 3 {
		t.Errorf("%d POSTs before the opening, want at least the threshold 3", posts)
	}
	got := lines(t, openedLine, a, b)
	if len(got) != 1 || got[0]["generation"] != float64(opened.generation) || got[0]["cooldown"] != "1s" {
		t.Errorf("opening lines %v, want one with generation %d and cooldown 1s", got, opened.generation)
	}
	t.Logf("%d POSTs before the opening; none in the %s that followed", posts, now.openFor.Round(time.Millisecond))
}

// TestBreakerSingleProbe: the breaker is open and its cooldown ends while
// both workers keep trying to claim. Exactly one POST, the probe, reaches the
// provider while the probe is held (K34).
func TestBreakerSingleProbe(t *testing.T) {
	pool := testdb.New(t)
	for _, ch := range domain.Channels {
		insertOn(t, pool, ch, 5)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE provider_breaker SET generation = 1, open_until = clock_timestamp() + interval '500 milliseconds',
		        cooldown = interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	hold := newGate()
	first := make(chan struct{}, 1)
	var calls atomic.Int32
	rec, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			first <- struct{}{}
			hold.wait()
		}
		accepted(validAccepted)(w, r)
	})
	a, b := pair(t, pool, srv.URL+"/", breakerSettings("worker-a"), breakerSettings("worker-b"))
	hold.openAtEnd(t)

	receive(t, first, "the probe's POST")
	if st := readBreaker(t, pool); st.openFor > 0 || st.probeToken == "" {
		t.Fatalf("breaker %+v at the first POST: want the cooldown over and the slot taken", st)
	}
	time.Sleep(time.Second) // about 50 claim rounds per worker and channel
	if n := len(rec.all()); n != 1 {
		t.Fatalf("provider got %d POSTs while the probe was held, want exactly 1", n)
	}
	if c := statusCounts(t, pool); c["processing"] != 1 {
		t.Fatalf("statuses %v, want exactly one processing", c)
	}

	hold.open()
	waitAll(t, pool, "sent", 10*time.Second)
	if st := readBreaker(t, pool); st.open || st.probeToken != "" {
		t.Fatalf("breaker %+v, want closed", st)
	}
	if got := lines(t, closedLine, a, b); len(got) != 1 {
		t.Fatalf("closing lines %v, want one", got)
	}
}

// TestBreakerRecovers: after the breaker opened on 503, the provider answers
// 202. The probe closes the breaker and every waiting row ends sent (K34).
func TestBreakerRecovers(t *testing.T) {
	pool := testdb.New(t)
	for _, ch := range domain.Channels {
		insertOn(t, pool, ch, 10)
	}
	answer := newSwitchable(status(http.StatusServiceUnavailable))
	_, srv := newProvider(t, answer.serve)
	a, b := pair(t, pool, srv.URL+"/", breakerSettings("worker-a"), breakerSettings("worker-b"))

	opened := waitBreaker(t, pool, 10*time.Second, "open", func(b breakerState) bool { return b.open })
	answer.set(status(http.StatusAccepted))
	waitAll(t, pool, "sent", 15*time.Second)

	st := readBreaker(t, pool)
	if st.open || st.failures != 0 || st.cooldown != time.Second || st.probeToken != "" || st.generation != opened.generation {
		t.Fatalf("breaker %+v, want closed in generation %d, count 0, cooldown 1s, no probe", st, opened.generation)
	}
	got := lines(t, closedLine, a, b)
	if len(got) != 1 || got[0]["generation"] != float64(opened.generation) || got[0]["cooldown"] != "1s" ||
		got[0]["breaker_probe"] != true {
		t.Fatalf("closing lines %v, want one from the probe with generation %d and cooldown 1s", got, opened.generation)
	}
}

// TestBreakerProbeFails: every probe gets 503. Each one reopens the breaker
// in a new generation with the cooldown doubled: 2 s, then 3 s, the maximum
// in this test, not 4 s (K34).
func TestBreakerProbeFails(t *testing.T) {
	pool := testdb.New(t)
	for _, ch := range domain.Channels {
		insertOn(t, pool, ch, 10)
	}
	_, srv := newProvider(t, status(http.StatusServiceUnavailable))
	sa, sb := breakerSettings("worker-a"), breakerSettings("worker-b")
	sa.cfg.Breaker.MaxCooldown = 3 * time.Second
	sb.cfg.Breaker.MaxCooldown = 3 * time.Second
	a, b := pair(t, pool, srv.URL+"/", sa, sb)

	deadline := time.Now().Add(15 * time.Second)
	var reopened []map[string]any
	for len(reopened) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("reopening lines after 15 s: %v, want two", reopened)
		}
		time.Sleep(50 * time.Millisecond)
		reopened = lines(t, reopenedLine, a, b)
	}
	opened := lines(t, openedLine, a, b)
	if len(opened) != 1 {
		t.Fatalf("opening lines %v, want one", opened)
	}
	slices.SortFunc(reopened, func(x, y map[string]any) int { return int(x["generation"].(float64) - y["generation"].(float64)) })
	gen := opened[0]["generation"].(float64)
	for i, want := range []string{"2s", "3s"} {
		r := reopened[i]
		if r["generation"] != gen+float64(i+1) || r["cooldown"] != want || r["breaker_probe"] != true {
			t.Errorf("reopening %d: %v, want generation %v, cooldown %s, from the probe", i+1, r, gen+float64(i+1), want)
		}
	}
	if st := readBreaker(t, pool); st.cooldown > 3*time.Second {
		t.Errorf("cooldown %s, want at most the 3 s maximum", st.cooldown)
	}
}

// TestProbePermanentError: the probe gets 400 (permanent) or 404 (terminal,
// K41). Its row fails; the slot is freed at once with the generation, the
// count and the cooldown unchanged, and the next claim becomes the probe
// (K7, K34, K41).
func TestProbePermanentError(t *testing.T) {
	for _, tc := range []struct {
		code      int
		lastError string
	}{
		{http.StatusBadRequest, "permanent: status 400"},
		{http.StatusNotFound, "terminal: unexpected status 404"},
	} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			pool := testdb.New(t)
			insertContent(t, pool, domain.ChannelSMS, "first")
			insertContent(t, pool, domain.ChannelEmail, "second")
			before := openAfterCooldown(t, pool)

			hold := newGate()
			second := make(chan time.Time, 1)
			var (
				calls   atomic.Int32
				firstAt atomic.Int64
			)
			_, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				switch calls.Add(1) {
				case 1:
					firstAt.Store(time.Now().UnixNano())
					w.WriteHeader(tc.code)
				case 2:
					second <- time.Now()
					hold.wait()
					accepted(validAccepted)(w, r)
				default:
					accepted(validAccepted)(w, r)
				}
			})
			claims := newClaimLog()
			sa, sb := breakerSettings("worker-a"), breakerSettings("worker-b")
			sa.hooks.AfterClaim, sb.hooks.AfterClaim = claims.hook("worker-a"), claims.hook("worker-b")
			a, b := pair(t, pool, srv.URL+"/", sa, sb)
			hold.openAtEnd(t)

			at := receive(t, second, "the next probe's POST")
			if gap := at.Sub(time.Unix(0, firstAt.Load())); gap > time.Second {
				t.Errorf("next probe %s after the first answer, want within 1 s", gap)
			}
			probes := claims.probes()
			if len(probes) != 2 || probes[0].claim.LeaseToken == probes[1].claim.LeaseToken {
				t.Fatalf("probe claims %+v, want two with different tokens", probes)
			}
			r := readRow(t, pool, probes[0].claim.ID)
			if r.status != "failed" || r.lastError == nil || *r.lastError != tc.lastError {
				t.Errorf("first probe's row %+v, want failed with %q", r, tc.lastError)
			}
			st := readBreaker(t, pool)
			if !st.open || st.generation != before.generation || st.failures != before.failures ||
				st.cooldown != before.cooldown || st.probeToken != probes[1].claim.LeaseToken {
				t.Errorf("breaker %+v, want open as before %+v with the second probe's token", st, before)
			}

			hold.open()
			waitStatus(t, pool, probes[1].claim.ID, "sent", 5*time.Second)
			st = waitBreaker(t, pool, 5*time.Second, "closed", func(b breakerState) bool { return !b.open })
			if st.generation != before.generation || st.cooldown != time.Second {
				t.Errorf("breaker %+v, want closed in generation %d with cooldown 1s", st, before.generation)
			}
			if n := len(lines(t, closedLine, a, b)); n != 1 {
				t.Errorf("%d closing lines, want 1", n)
			}
		})
	}
}

// TestProbeTooManyRequestsFreesSlot: under the standard profile the probe
// gets 429 with Retry-After: 5. Its row goes back to queued for at least 5
// seconds, the slot is freed in the same transaction, and within 1 second,
// long before the probe's 15-second lease would end, the other worker's
// claim becomes the new probe; the generation, the count and the cooldown
// stay as they were (K7, K34).
func TestProbeTooManyRequestsFreesSlot(t *testing.T) {
	pool := testdb.New(t)
	for _, ch := range domain.Channels {
		insertOn(t, pool, ch, 2)
	}
	before := openAfterCooldown(t, pool)

	hold := newGate()
	second := make(chan time.Time, 1)
	var (
		calls   atomic.Int32
		firstAt atomic.Int64
	)
	_, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			firstAt.Store(time.Now().UnixNano())
			tooMany("5")(w, r)
		case 2:
			second <- time.Now()
			hold.wait()
			accepted(validAccepted)(w, r)
		default:
			accepted(validAccepted)(w, r)
		}
	})

	// One call slot per worker. The worker whose probe got the 429 keeps its
	// slot after the write until the test ends, so the new probe can only
	// come from the other worker.
	release := newGate()
	var once sync.Once
	keepSlot := func(f domain.Finish, _ domain.Finished) {
		if f.Attempt.Probe && f.Result.LastError == "temporary: status 429" {
			once.Do(release.wait)
		}
	}
	claims := newClaimLog()
	sinkA, sinkB := &finishLog{}, &finishLog{}
	sa, sb := breakerSettings("worker-a"), breakerSettings("worker-b")
	sa.cfg.InFlight, sb.cfg.InFlight = 1, 1
	sa.hooks.AfterClaim, sb.hooks.AfterClaim = claims.hook("worker-a"), claims.hook("worker-b")
	sa.wrap, sb.wrap = spyOn(sinkA, keepSlot), spyOn(sinkB, keepSlot)
	a, b := pair(t, pool, srv.URL+"/", sa, sb)
	hold.openAtEnd(t)
	release.openAtEnd(t)

	at := receive(t, second, "the new probe's POST")
	if gap := at.Sub(time.Unix(0, firstAt.Load())); gap > time.Second {
		t.Errorf("new probe %s after the 429, want within 1 s", gap)
	}
	probes := claims.probes()
	if len(probes) != 2 || probes[0].worker == probes[1].worker {
		t.Fatalf("probe claims %+v, want two from different workers", probes)
	}
	var wait time.Duration
	if err := pool.QueryRow(t.Context(),
		`SELECT available_at - updated_at FROM notifications WHERE id = $1::uuid AND status = 'queued'`,
		probes[0].claim.ID).Scan(&wait); err != nil {
		t.Fatalf("first probe's row: %v, want queued", err)
	}
	if wait < 5*time.Second {
		t.Errorf("first probe's row due %s after its write, want at least 5 s", wait)
	}
	st := readBreaker(t, pool)
	if !st.open || st.generation != before.generation || st.failures != before.failures ||
		st.cooldown != before.cooldown || st.probeToken != probes[1].claim.LeaseToken {
		t.Errorf("breaker %+v, want open as before %+v with the new probe's token", st, before)
	}

	hold.open()
	waitStatus(t, pool, probes[1].claim.ID, "sent", 5*time.Second)
	waitBreaker(t, pool, 5*time.Second, "closed", func(b breakerState) bool { return !b.open })
	release.open()
	t.Logf("new probe %s after the 429, from %s; the first came from %s",
		at.Sub(time.Unix(0, firstAt.Load())).Round(time.Millisecond), probes[1].worker, probes[0].worker)
	_, _ = a, b
}

// TestProbeMissedStartFreesSlot: the first probe is stopped before the start
// gate and released after its start deadline (K42). It sends nothing; its
// give-back frees the slot at once, the next claim takes a new probe, and
// the provider receives exactly one POST, from the new probe (K34).
func TestProbeMissedStartFreesSlot(t *testing.T) {
	pool := testdb.New(t)
	id := insertContent(t, pool, domain.ChannelSMS, "only")
	openAfterCooldown(t, pool)
	rec, srv := newProvider(t, accepted(validAccepted))

	var once sync.Once
	stall := func(c domain.Claim) {
		if c.Probe {
			once.Do(func() { time.Sleep(250 * time.Millisecond) }) // past the 100 ms grace
		}
	}
	claims := newClaimLog()
	sa, sb := breakerSettings("worker-a"), breakerSettings("worker-b")
	sa.hooks.BeforeGate, sb.hooks.BeforeGate = stall, stall
	sa.hooks.AfterClaim, sb.hooks.AfterClaim = claims.hook("worker-a"), claims.hook("worker-b")
	a, b := pair(t, pool, srv.URL+"/", sa, sb)

	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	settle()
	if n := len(rec.all()); n != 1 {
		t.Fatalf("provider got %d POSTs, want exactly 1, from the new probe", n)
	}
	probes := claims.probes()
	if len(probes) != 2 || probes[0].claim.LeaseToken == probes[1].claim.LeaseToken {
		t.Fatalf("probe claims %+v, want two with different tokens", probes)
	}
	missed := lines(t, missedLine+", no request sent, notification given back", a, b)
	if len(missed) != 1 || missed[0]["breaker_probe"] != true {
		t.Fatalf("missed-deadline lines %v, want one from the first probe", missed)
	}
	// The stall was 250 ms; the new probe followed well within a second of
	// the give-back, not at the 15-second lease's end.
	if gap := probes[1].at.Sub(probes[0].at); gap > time.Second {
		t.Errorf("new probe %s after the first, want within 1 s", gap)
	}
	if r.attempts != 1 {
		t.Errorf("attempts %d, want 1: the missed start's attempt was returned", r.attempts)
	}
	if st := readBreaker(t, pool); st.open || st.probeToken != "" {
		t.Errorf("breaker %+v, want closed by the new probe", st)
	}
}

// TestLateAcceptedResult: a call that started before the opening answers 202
// after it, while its lease is valid. Its row is sent; the breaker stays open
// and its count does not change (K3, K34).
func TestLateAcceptedResult(t *testing.T) {
	pool := testdb.New(t)
	lateID := insertContent(t, pool, domain.ChannelSMS, "late")
	insertOn(t, pool, domain.ChannelEmail, 5)
	insertOn(t, pool, domain.ChannelPush, 5)

	hold := newGate()
	lateArrived := make(chan struct{}, 1)
	_, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if content(r) == "late" {
			lateArrived <- struct{}{}
			hold.wait()
			accepted(validAccepted)(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	sink := &finishLog{}
	sa, sb := breakerSettings("worker-a"), breakerSettings("worker-b")
	sa.wrap, sb.wrap = spyOn(sink, nil), spyOn(sink, nil)
	pair(t, pool, srv.URL+"/", sa, sb)
	hold.openAtEnd(t)

	receive(t, lateArrived, "the late call")
	opened := waitBreaker(t, pool, 10*time.Second, "open", func(b breakerState) bool { return b.open })
	hold.open()
	waitStatus(t, pool, lateID, "sent", 5*time.Second)

	st := readBreaker(t, pool)
	if !sameBreaker(st, opened) {
		t.Fatalf("breaker %+v after the late 202, want unchanged %+v", st, opened)
	}
	calls := sink.of(lateID)
	if len(calls) != 1 || !calls[0].fin.Written || calls[0].fin.Breaker != domain.BreakerUnchanged ||
		calls[0].f.Attempt.Generation >= opened.generation {
		t.Fatalf("late result writes %+v, want one, written, of an older generation, breaker unchanged", calls)
	}
}

// staleProbe runs probe A to its answer, holds A before its result write,
// and ends A's probe lease by the database clock. It returns A's claim, the
// gate that releases A's write, and the results A writes.
type staleProbe struct {
	pool     *pgxpool.Pool
	a        *running
	claimA   domain.Claim
	releaseA *gate
	sinkA    *finishLog
}

func startStaleProbe(t *testing.T, pool *pgxpool.Pool, url string) *staleProbe {
	t.Helper()
	atWrite := make(chan domain.Claim, 1)
	release := newGate()
	sink := &finishLog{}
	s := breakerSettings("worker-a")
	s.cfg.InFlight = 1
	s.wrap = spyWithoutRecovery(sink) // the expired row stays as A left it
	s.hooks.BeforeWrite = func(c domain.Claim) {
		atWrite <- c
		release.wait()
	}
	a := startWorker(t, pool, url, s)
	release.openAtEnd(t)
	c := receive(t, atWrite, "probe A's answer")
	if !c.Probe {
		t.Fatalf("A's claim %+v, want the probe", c)
	}
	expireProbeLease(t, pool, c.ID)
	return &staleProbe{pool: pool, a: a, claimA: c, releaseA: release, sinkA: sink}
}

// writeA releases A's result write and returns what it did.
func (p *staleProbe) writeA(t *testing.T) finishCall {
	t.Helper()
	p.releaseA.open()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls := p.sinkA.of(p.claimA.ID); len(calls) > 0 {
			return calls[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("A's result write never returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkNotWritten checks that A's row is still processing under A's token.
func (p *staleProbe) checkNotWritten(t *testing.T) {
	t.Helper()
	r := readRow(t, p.pool, p.claimA.ID)
	if r.status != "processing" || r.token == nil || *r.token != p.claimA.LeaseToken {
		t.Errorf("A's row %+v, want still processing under A's token: not written", r)
	}
}

// TestStaleProbeResultNoTransition: probe A's lease expires, probe B takes
// the slot, then A answers 202 or 503. A's answer neither closes nor reopens
// the breaker nor changes its cooldown, and A's row is not written; B's
// answer is the one that moves the breaker (K3, K34).
func TestStaleProbeResultNoTransition(t *testing.T) {
	for _, code := range []int{http.StatusAccepted, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			pool := testdb.New(t)
			insertContent(t, pool, domain.ChannelSMS, "a")
			insertContent(t, pool, domain.ChannelEmail, "b")
			before := openAfterCooldown(t, pool)

			holdB := newGate()
			arrivedB := make(chan struct{}, 1)
			var calls atomic.Int32
			_, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 2 {
					arrivedB <- struct{}{}
					holdB.wait()
				}
				status(code)(w, r)
			})
			p := startStaleProbe(t, pool, srv.URL+"/")

			sinkB := &finishLog{}
			sb := breakerSettings("worker-b")
			sb.wrap = spyWithoutRecovery(sinkB)
			b := startWorker(t, secondPool(t, pool), srv.URL+"/", sb)
			holdB.openAtEnd(t)
			receive(t, arrivedB, "probe B's POST")
			withB := readBreaker(t, pool)
			if withB.probeToken == "" || withB.probeToken == p.claimA.LeaseToken {
				t.Fatalf("breaker %+v, want B's probe in the slot", withB)
			}

			got := p.writeA(t)
			if got.err != nil || got.fin.Written || got.fin.Breaker != domain.BreakerUnchanged {
				t.Errorf("A's write %+v, want not written and the breaker unchanged", got)
			}
			if st := readBreaker(t, pool); !sameBreaker(st, withB) {
				t.Errorf("breaker %+v after A's answer, want unchanged %+v", st, withB)
			}
			p.checkNotWritten(t)

			holdB.open()
			want := map[int]struct {
				line     string
				open     bool
				gen      int64
				cooldown time.Duration
			}{
				http.StatusAccepted:           {closedLine, false, before.generation, time.Second},
				http.StatusServiceUnavailable: {reopenedLine, true, before.generation + 1, 4 * time.Second},
			}[code]
			st := waitBreaker(t, pool, 5*time.Second, "moved by B", func(s breakerState) bool {
				return s.generation != withB.generation || s.open != withB.open
			})
			if st.open != want.open || st.generation != want.gen || st.cooldown != want.cooldown || st.probeToken != "" {
				t.Errorf("breaker %+v after B's answer, want open=%v generation %d cooldown %s", st, want.open, want.gen, want.cooldown)
			}
			if n := len(lines(t, want.line, b)); n != 1 {
				t.Errorf("B wrote %d %q lines, want 1", n, want.line)
			}
			if n := len(lines(t, want.line, p.a)); n != 0 {
				t.Errorf("A wrote %d %q lines, want 0", n, want.line)
			}
		})
	}
}

// TestExpiredProbeLeaseNoTransition: probe A's lease expires and no new probe
// has taken the slot (worker B finds no other queued row); then A answers
// 202 or 503. Neither closes, reopens or changes the cooldown, and A's row is
// not written (K3, K34).
func TestExpiredProbeLeaseNoTransition(t *testing.T) {
	for _, code := range []int{http.StatusAccepted, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			pool := testdb.New(t)
			insertContent(t, pool, domain.ChannelSMS, "a")
			before := openAfterCooldown(t, pool)
			rec, srv := newProvider(t, status(code))
			p := startStaleProbe(t, pool, srv.URL+"/")

			sb := breakerSettings("worker-b")
			sb.wrap = spyWithoutRecovery(&finishLog{})
			b := startWorker(t, secondPool(t, pool), srv.URL+"/", sb)
			time.Sleep(200 * time.Millisecond) // B tries to claim and finds nothing
			if st := readBreaker(t, pool); st.probeToken != p.claimA.LeaseToken {
				t.Fatalf("breaker %+v, want A's expired probe still in the slot", st)
			}

			got := p.writeA(t)
			if got.err != nil || got.fin.Written || got.fin.Breaker != domain.BreakerUnchanged {
				t.Errorf("A's write %+v, want not written and the breaker unchanged", got)
			}
			st := readBreaker(t, pool)
			if !st.open || st.generation != before.generation || st.cooldown != before.cooldown || st.failures != before.failures {
				t.Errorf("breaker %+v after A's answer, want as before %+v", st, before)
			}
			p.checkNotWritten(t)
			if n := len(rec.all()); n != 1 {
				t.Errorf("provider got %d POSTs, want 1, A's", n)
			}
			for _, line := range []string{openedLine, reopenedLine, closedLine} {
				if n := len(lines(t, line, p.a, b)); n != 0 {
					t.Errorf("%d %q lines, want 0", n, line)
				}
			}
		})
	}
}

// TestCrashedProbe: the probe's worker stops after its claim and never
// writes a result, as if it had crashed. Until the probe's lease ends nobody
// sends; after probe_expires_at, recovery takes the row back and another
// worker's claim becomes the probe, which closes the breaker (K9, K34).
func TestCrashedProbe(t *testing.T) {
	pool := testdb.New(t)
	id := insertContent(t, pool, domain.ChannelSMS, "only")
	openAfterCooldown(t, pool)
	rec, srv := newProvider(t, accepted(validAccepted))

	crashed := newGate()
	claimedA := make(chan domain.Claim, 1)
	sa := breakerSettings("worker-a")
	sa.cfg.InFlight = 1 // the stopped delivery holds A's only call slot
	sa.cfg.LeaseDuration = 1500 * time.Millisecond
	sa.cfg.ResultBudget = 500 * time.Millisecond
	sa.hooks.AfterClaim = func(c domain.Claim) {
		claimedA <- c
		crashed.wait()
	}
	startWorker(t, pool, srv.URL+"/", sa)
	crashed.openAtEnd(t)
	ca := receive(t, claimedA, "A's probe claim")
	if !ca.Probe {
		t.Fatalf("A's claim %+v, want the probe", ca)
	}

	claims := newClaimLog()
	sb := breakerSettings("worker-b")
	sb.hooks.AfterClaim = claims.hook("worker-b")
	b := startWorker(t, secondPool(t, pool), srv.URL+"/", sb)

	// While A's probe holds the slot by the database clock, nobody sends. The
	// POSTs are counted before the slot is read, so a POST counted while A
	// still held it arrived before A's probe lease ended.
	for {
		posts := len(rec.all())
		var held bool
		if err := pool.QueryRow(t.Context(),
			`SELECT coalesce(probe_token = $1::uuid AND probe_expires_at > clock_timestamp(), false)
			 FROM provider_breaker`, ca.LeaseToken).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if !held {
			break
		}
		if posts != 0 {
			t.Fatalf("provider got %d POSTs while A's probe held the slot, want 0", posts)
		}
		time.Sleep(20 * time.Millisecond)
	}

	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	probes := claims.probes()
	if len(probes) != 1 || probes[0].claim.ID != id || probes[0].claim.LeaseToken == ca.LeaseToken {
		t.Fatalf("B's probe claims %+v, want one on %s with a new token", probes, id)
	}
	if r.attempts != 2 || len(rec.all()) != 1 {
		t.Errorf("attempts %d and %d POSTs, want 2 (A's counted) and 1", r.attempts, len(rec.all()))
	}
	if st := readBreaker(t, pool); st.open || st.probeToken != "" {
		t.Errorf("breaker %+v, want closed by B's probe", st)
	}
	if n := len(lines(t, closedLine, b)); n != 1 {
		t.Errorf("B wrote %d closing lines, want 1", n)
	}
}

// TestRetryAfterDelaysOnlyItsNotification: under the standard profile one
// row gets 429 with Retry-After: 5. Only that row waits; the others keep
// being claimed and sent, and the breaker stays closed (K7, K34).
func TestRetryAfterDelaysOnlyItsNotification(t *testing.T) {
	pool := testdb.New(t)
	limitedID := insertContent(t, pool, domain.ChannelSMS, "limited")
	insertOn(t, pool, domain.ChannelSMS, 3)
	insertOn(t, pool, domain.ChannelEmail, 3)
	insertOn(t, pool, domain.ChannelPush, 3)
	_, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if content(r) == "limited" {
			tooMany("5")(w, r)
			return
		}
		accepted(validAccepted)(w, r)
	})
	a, b := pair(t, pool, srv.URL+"/", breakerSettings("worker-a"), breakerSettings("worker-b"))

	deadline := time.Now().Add(4 * time.Second)
	for statusCounts(t, pool)["sent"] < 9 {
		if time.Now().After(deadline) {
			t.Fatalf("statuses %v within 4 s, want the other nine sent", statusCounts(t, pool))
		}
		time.Sleep(20 * time.Millisecond)
	}
	r := readRow(t, pool, limitedID)
	if r.status != "queued" || r.attempts != 1 {
		t.Errorf("limited row %+v, want queued after one attempt", r)
	}
	var wait time.Duration
	if err := pool.QueryRow(t.Context(), `SELECT available_at - updated_at FROM notifications WHERE id = $1::uuid`,
		limitedID).Scan(&wait); err != nil || wait < 5*time.Second {
		t.Errorf("limited row due %s after its write (err %v), want at least 5 s", wait, err)
	}
	if st := readBreaker(t, pool); st.open || st.generation != 0 || st.failures != 0 {
		t.Errorf("breaker %+v, want closed and untouched", st)
	}
	if n := len(lines(t, openedLine, a, b)); n != 0 {
		t.Errorf("%d opening lines, want 0", n)
	}
}

// TestSharedQuotaOpensBreaker: under a test profile that states a shared
// quota, a 429 with Retry-After: 5 opens the breaker until that time. A
// second 429, from a call that started before the opening, says Retry-After:
// 1 and does not shorten the pause (K34).
func TestSharedQuotaOpensBreaker(t *testing.T) {
	pool := testdb.New(t)
	insertContent(t, pool, domain.ChannelSMS, "five")
	oneID := insertContent(t, pool, domain.ChannelEmail, "one")

	bothIn := newGate()
	answerOne := newGate()
	var calls atomic.Int32
	_, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			bothIn.open()
		}
		bothIn.wait() // both calls are in flight before either answers
		if content(r) == "five" {
			tooMany("5")(w, r)
			return
		}
		answerOne.wait()
		tooMany("1")(w, r)
	})
	sa, sb := breakerSettings("worker-a"), breakerSettings("worker-b")
	sa.cfg.Profile, sb.cfg.Profile = provider.ProfileSharedQuotaTest, provider.ProfileSharedQuotaTest
	a, b := pair(t, pool, srv.URL+"/", sa, sb)
	bothIn.openAtEnd(t)
	answerOne.openAtEnd(t)

	opened := waitBreaker(t, pool, 10*time.Second, "open", func(b breakerState) bool { return b.open })
	if opened.openFor < 4500*time.Millisecond || opened.openFor > 5*time.Second {
		t.Fatalf("breaker open for %s more, want about 5 s", opened.openFor)
	}
	var openUntil time.Time
	if err := pool.QueryRow(t.Context(), `SELECT open_until FROM provider_breaker`).Scan(&openUntil); err != nil {
		t.Fatal(err)
	}

	answerOne.open()
	waitStatus(t, pool, oneID, "queued", 5*time.Second)
	var after time.Time
	if err := pool.QueryRow(t.Context(), `SELECT open_until FROM provider_breaker`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(openUntil) {
		t.Errorf("open_until moved from %s to %s after Retry-After: 1, want unchanged", openUntil, after)
	}
	got := lines(t, openedLine, a, b)
	if len(got) != 1 || got[0]["generation"] != float64(opened.generation) {
		t.Errorf("opening lines %v, want one", got)
	}
}
