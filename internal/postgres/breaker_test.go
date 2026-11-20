package postgres_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

// policy is the breaker's settings in these tests: threshold 3, cooldown 1 s.
var policy = domain.BreakerPolicy{Threshold: 3, Cooldown: time.Second, MaxCooldown: 4 * time.Second}

// breakerRow is the provider_breaker row as text, "" for null, so two
// readings compare exactly.
type breakerRow struct {
	generation int64
	failures   int
	openUntil  string
	cooldown   string
	probeToken string
	probeEnds  string
}

func readBreaker(t *testing.T, pool *pgxpool.Pool) breakerRow {
	t.Helper()
	var b breakerRow
	err := pool.QueryRow(t.Context(),
		`SELECT generation, consecutive_failures, coalesce(open_until::text, ''), cooldown::text,
		        coalesce(probe_token::text, ''), coalesce(probe_expires_at::text, '')
		 FROM provider_breaker`).Scan(
		&b.generation, &b.failures, &b.openUntil, &b.cooldown, &b.probeToken, &b.probeEnds)
	if err != nil {
		t.Fatalf("read breaker: %v", err)
	}
	return b
}

// openBreaker opens the breaker in the next generation with its cooldown
// ending at the database clock plus untilIn (negative: already over).
func openBreaker(t *testing.T, pool *pgxpool.Pool, untilIn time.Duration) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE provider_breaker
		 SET generation = generation + 1, consecutive_failures = 0, cooldown = interval '1 second',
		     open_until = clock_timestamp() + $1::bigint * interval '1 microsecond'`,
		untilIn.Microseconds()); err != nil {
		t.Fatal(err)
	}
}

func outage(c domain.Claim) domain.Finish {
	return domain.Finish{
		Attempt: domain.AttemptOf(c),
		Result:  domain.Result{Kind: domain.ResultRetry, LastError: "temporary: status 503", Outage: true},
		Policy:  policy,
	}
}

// TestProbeSlotFreeWithoutWork: with the breaker open and its cooldown over,
// a claim that finds no notification, or no permit, writes nothing: the
// probe slot stays free (K34).
func TestProbeSlotFreeWithoutWork(t *testing.T) {
	t.Run("no queued rows", func(t *testing.T) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		openBreaker(t, pool, -time.Second)
		before := readBreaker(t, pool)

		for _, ch := range domain.Channels {
			res, err := store.ClaimNext(t.Context(), request(ch))
			if err != nil || res.Outcome != domain.ClaimEmpty {
				t.Fatalf("claim on %s: %+v err=%v, want empty", ch, res, err)
			}
		}
		if after := readBreaker(t, pool); after != before || after.probeToken != "" {
			t.Fatalf("breaker %+v, want unchanged %+v with no probe token", after, before)
		}
	})

	t.Run("permit not due", func(t *testing.T) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "waiting", -time.Second)
		if _, err := pool.Exec(t.Context(),
			`UPDATE channel_limits SET next_permit_at = clock_timestamp() + interval '1 minute' WHERE channel = 'sms'`); err != nil {
			t.Fatal(err)
		}
		openBreaker(t, pool, -time.Second)
		before := readBreaker(t, pool)

		res, err := store.ClaimNext(t.Context(), limited(domain.ChannelSMS))
		if err != nil || res.Outcome != domain.ClaimThrottled {
			t.Fatalf("claim: %+v err=%v, want throttled", res, err)
		}
		if after := readBreaker(t, pool); after != before || after.probeToken != "" {
			t.Fatalf("breaker %+v, want unchanged %+v with no probe token", after, before)
		}
	})

	t.Run("then a queued row becomes the probe", func(t *testing.T) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		openBreaker(t, pool, -time.Second)
		if res, err := store.ClaimNext(t.Context(), request(domain.ChannelSMS)); err != nil || res.Outcome != domain.ClaimEmpty {
			t.Fatalf("claim: %+v err=%v, want empty", res, err)
		}
		id := insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "probe", -time.Second)
		res, err := store.ClaimNext(t.Context(), request(domain.ChannelSMS))
		if err != nil || res.Outcome != domain.ClaimClaimed || !res.Claim.Probe || res.Claim.ID != id {
			t.Fatalf("claim: %+v err=%v, want the probe on %s", res, err, id)
		}
		b := readBreaker(t, pool)
		r := readLease(t, pool, id)
		if b.probeToken != res.Claim.LeaseToken || r.expires == nil {
			t.Fatalf("breaker %+v, row %+v: want the probe token to be the lease token", b, r)
		}
		var sameEnd bool
		if err := pool.QueryRow(t.Context(),
			`SELECT b.probe_expires_at = n.lease_expires_at FROM provider_breaker AS b, notifications AS n
			 WHERE n.id = $1::uuid`, id).Scan(&sameEnd); err != nil || !sameEnd {
			t.Fatalf("probe_expires_at equals the lease end: %v err=%v, want true", sameEnd, err)
		}
		// Every other claim, on any channel, is paused while the probe holds
		// the slot.
		insertOn(t, pool, domain.ChannelEmail, domain.PriorityNormal, "other", -time.Second)
		if res, err := store.ClaimNext(t.Context(), request(domain.ChannelEmail)); err != nil ||
			res.Outcome != domain.ClaimPaused || res.Wait != 0 {
			t.Fatalf("second claim: %+v err=%v, want paused with no wait", res, err)
		}
	})
}

// TestOldGenerationFailureIgnored: an outage failure that carries the
// previous generation is written to its notification but does not change
// the failure count; one of the current generation does (K34).
func TestOldGenerationFailureIgnored(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	insertQueued(t, pool, 2)
	old := mustClaim(t, store)
	if old.Generation != 0 {
		t.Fatalf("generation %d, want 0", old.Generation)
	}
	// The breaker opened and closed meanwhile: generation 1, one failure.
	if _, err := pool.Exec(t.Context(),
		`UPDATE provider_breaker SET generation = 1, consecutive_failures = 1, open_until = NULL`); err != nil {
		t.Fatal(err)
	}

	fin, err := store.Finish(t.Context(), outage(old))
	if err != nil || !fin.Written || fin.Breaker != domain.BreakerUnchanged {
		t.Fatalf("old generation's failure: %+v err=%v, want written, breaker unchanged", fin, err)
	}
	if b := readBreaker(t, pool); b.generation != 1 || b.failures != 1 {
		t.Fatalf("breaker %+v, want generation 1 with one failure", b)
	}
	if r := readLease(t, pool, old.ID); r.status != "queued" {
		t.Fatalf("row %+v, want queued again", r)
	}

	current := mustClaim(t, store)
	if current.Generation != 1 {
		t.Fatalf("generation %d, want 1", current.Generation)
	}
	if fin, err := store.Finish(t.Context(), outage(current)); err != nil || !fin.Written {
		t.Fatalf("current generation's failure: %+v err=%v", fin, err)
	}
	if b := readBreaker(t, pool); b.failures != 2 {
		t.Fatalf("breaker %+v, want two failures", b)
	}
}

// TestRecoveryWhileOpen: with the breaker open, recovery returns a row whose
// lease expired to queued and leaves the breaker row as it was (K9, K34).
func TestRecoveryWhileOpen(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	id := insertQueued(t, pool, 1)[0]
	mustClaim(t, store)
	openBreaker(t, pool, time.Minute)
	expire(t, pool, id)
	before := readBreaker(t, pool)

	got := recoverAll(t, store)
	if len(got) != 1 || got[0].ID != id || got[0].Status != domain.StatusQueued {
		t.Fatalf("recovered %+v, want %s back to queued", got, id)
	}
	if r := readLease(t, pool, id); r.status != "queued" || r.token != nil {
		t.Fatalf("row %+v, want queued without a lease", r)
	}
	if after := readBreaker(t, pool); after != before {
		t.Fatalf("breaker %+v, want unchanged %+v", after, before)
	}
}

// TestBreakerConcurrentClaimsAndResults: sixteen claimers on three channels
// claim and write random results, the probe's included, while the breaker
// opens, reopens and closes. No statement fails, so no PostgreSQL deadlock
// error (SQLSTATE 40P01) occurs (K34's lock order).
func TestBreakerConcurrentClaimsAndResults(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
		 SELECT CASE g % 3 WHEN 0 THEN '+905551234567' WHEN 1 THEN 'a@example.com' ELSE 'device-token-1' END,
		        (ARRAY['sms','email','push'])[1 + g % 3], 'load', 'normal', 'corr'
		 FROM generate_series(1, 6000) AS g`); err != nil {
		t.Fatal(err)
	}
	fast := domain.BreakerPolicy{Threshold: 3, Cooldown: 5 * time.Millisecond, MaxCooldown: 20 * time.Millisecond}
	req := func(ch domain.Channel) domain.ClaimRequest {
		r := request(ch)
		r.MaxAttempts = 1000
		return r
	}

	var (
		changes          sync.Map // domain.BreakerChange → *atomic.Int64
		probes, finishes atomic.Int64
		firstErr         error
		errOnce          sync.Once
	)
	count := func(c domain.BreakerChange) {
		v, _ := changes.LoadOrStore(c, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	}
	fail := func(err error) { errOnce.Do(func() { firstErr = err }) }

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			ch := domain.Channels[i%3]
			for ctx.Err() == nil {
				res, err := store.ClaimNext(context.Background(), req(ch))
				if err != nil {
					fail(err)
					return
				}
				if res.Outcome != domain.ClaimClaimed {
					continue
				}
				c := res.Claim
				if c.Probe {
					probes.Add(1)
				}
				var r domain.Result
				switch n := rand.IntN(10); {
				case n < 5:
					r = domain.Result{Kind: domain.ResultRetry, LastError: "temporary: status 503", Outage: true}
				case n < 9:
					r = domain.Result{Kind: domain.ResultSent}
				default:
					r = domain.Result{Kind: domain.ResultFailed, LastError: "permanent: status 400"}
				}
				fin, err := store.Finish(context.Background(), domain.Finish{Attempt: domain.AttemptOf(c), Result: r, Policy: fast})
				if err != nil {
					fail(err)
					return
				}
				finishes.Add(1)
				if fin.Breaker != domain.BreakerUnchanged {
					count(fin.Breaker)
				}
			}
		})
	}
	wg.Wait()

	var pgErr *pgconn.PgError
	if errors.As(firstErr, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("deadlock: %v", firstErr)
	}
	if firstErr != nil {
		t.Fatalf("claim or result write failed: %v", firstErr)
	}
	got := map[domain.BreakerChange]int64{}
	changes.Range(func(k, v any) bool {
		got[k.(domain.BreakerChange)] = v.(*atomic.Int64).Load()
		return true
	})
	t.Logf("results=%d probes=%d breaker changes=%v", finishes.Load(), probes.Load(), got)
	for _, c := range []domain.BreakerChange{domain.BreakerOpened, domain.BreakerReopened, domain.BreakerClosed} {
		if got[c] == 0 {
			t.Errorf("the breaker was never %s; the run did not exercise it", c)
		}
	}
}
