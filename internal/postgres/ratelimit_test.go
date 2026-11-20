package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

var recipients = map[domain.Channel]string{
	domain.ChannelSMS:   "+905551234567",
	domain.ChannelEmail: "user@example.com",
	domain.ChannelPush:  "device-token-1",
}

// insertOn inserts one queued notification on ch with priority and content,
// due at the database clock plus due (negative: already due), and returns
// its ID.
func insertOn(t *testing.T, pool *pgxpool.Pool, ch domain.Channel, priority domain.Priority, content string, due time.Duration) string {
	t.Helper()
	var id string
	err := pool.QueryRow(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, available_at)
		 VALUES ($1, $2, $3, $4, 'corr', clock_timestamp() + $5::bigint * interval '1 microsecond')
		 RETURNING id::text`, recipients[ch], string(ch), content, string(priority), due.Microseconds()).Scan(&id)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

func nextPermit(t *testing.T, pool *pgxpool.Pool, ch domain.Channel) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT next_permit_at FROM channel_limits WHERE channel = $1`, string(ch)).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// holdLimiter locks ch's limiter row in a transaction held until the end of
// the test, as another worker in the middle of its claim would.
func holdLimiter(t *testing.T, pool *pgxpool.Pool, ch domain.Channel) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM channel_limits WHERE channel = $1 FOR UPDATE`, string(ch)); err != nil {
		t.Fatal(err)
	}
}

// limited is a claim with the production permit settings.
func limited(ch domain.Channel) domain.ClaimRequest {
	req := request(ch)
	req.PermitInterval = 11 * time.Millisecond
	req.PermitGrace = 100 * time.Millisecond
	return req
}

// TestPermitKeptWithoutWork: on an empty channel the permit is due, the claim
// finds no notification, and the permit is not used up (K5).
func TestPermitKeptWithoutWork(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	before := nextPermit(t, pool, domain.ChannelSMS)

	res, err := store.ClaimNext(t.Context(), limited(domain.ChannelSMS))
	if err != nil || res.Outcome != domain.ClaimEmpty {
		t.Fatalf("claim: %+v err=%v, want empty", res, err)
	}
	if after := nextPermit(t, pool, domain.ChannelSMS); !after.Equal(before) {
		t.Fatalf("next_permit_at moved from %s to %s without a claim", before, after)
	}
}

// TestClaimOutcomes: a permit not yet due gives throttled with the wait and
// takes nothing; a limiter row held by another worker gives busy at once
// (K5).
func TestClaimOutcomes(t *testing.T) {
	t.Run("throttled", func(t *testing.T) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "first", -time.Second)
		second := insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "second", -time.Second)

		// Both claims share one 5-second budget; the second runs long before
		// the first one's next permit, a minute later, is due.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req := limited(domain.ChannelSMS)
		req.PermitInterval = time.Minute

		first, err := store.ClaimNext(ctx, req)
		if err != nil || first.Outcome != domain.ClaimClaimed {
			t.Fatalf("first claim: %+v err=%v, want claimed", first, err)
		}
		permit := nextPermit(t, pool, domain.ChannelSMS)
		got, err := store.ClaimNext(ctx, req)
		if err != nil {
			t.Fatalf("second claim within the shared budget: %v", err)
		}
		if got.Outcome != domain.ClaimThrottled || got.Wait <= 0 || got.Wait > time.Minute {
			t.Fatalf("second claim: %+v, want throttled with a wait in (0, 1m]", got)
		}
		t.Logf("throttled with a wait of %s", got.Wait)
		if after := nextPermit(t, pool, domain.ChannelSMS); !after.Equal(permit) {
			t.Fatalf("the throttled claim moved next_permit_at from %s to %s", permit, after)
		}
		if r := readLease(t, pool, second); r.status != "queued" || r.attempts != 0 || r.token != nil {
			t.Fatalf("second notification %+v, want queued with attempts 0 and no lease", r)
		}
	})

	t.Run("busy", func(t *testing.T) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "waiting", -time.Second)
		holdLimiter(t, pool, domain.ChannelSMS)

		start := time.Now()
		res, err := store.ClaimNext(t.Context(), limited(domain.ChannelSMS))
		took := time.Since(start)
		if err != nil || res.Outcome != domain.ClaimBusy {
			t.Fatalf("claim: %+v err=%v, want busy", res, err)
		}
		if took > time.Second {
			t.Fatalf("busy came back after %s, want at once", took)
		}
		t.Logf("busy came back after %s", took.Round(time.Microsecond))
	})
}

// TestChannelsIndependent: while another worker holds the sms limiter row,
// email and push claims still succeed (K5).
func TestChannelsIndependent(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	email := insertOn(t, pool, domain.ChannelEmail, domain.PriorityNormal, "e", -time.Second)
	push := insertOn(t, pool, domain.ChannelPush, domain.PriorityNormal, "p", -time.Second)
	insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "s", -time.Second)
	holdLimiter(t, pool, domain.ChannelSMS)

	if res, err := store.ClaimNext(t.Context(), limited(domain.ChannelSMS)); err != nil || res.Outcome != domain.ClaimBusy {
		t.Fatalf("sms claim: %+v err=%v, want busy", res, err)
	}
	for ch, id := range map[domain.Channel]string{domain.ChannelEmail: email, domain.ChannelPush: push} {
		res, err := store.ClaimNext(t.Context(), limited(ch))
		if err != nil || res.Outcome != domain.ClaimClaimed || res.Claim.ID != id {
			t.Fatalf("%s claim: %+v err=%v, want its notification claimed", ch, res, err)
		}
	}
}

// TestRecoveryWithoutPermit: recovery returns an expired sms lease to queued
// while the sms limiter row is held: it never touches channel_limits (K40).
func TestRecoveryWithoutPermit(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	id := insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "s", -time.Second)
	mustClaim(t, store)
	expire(t, pool, id)
	holdLimiter(t, pool, domain.ChannelSMS)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := store.RecoverExpired(ctx, maxAttempts, 100)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Status != domain.StatusQueued {
		t.Fatalf("recovered %+v err=%v, want the sms row back in queued", got, err)
	}
}

// claimOrder claims on ch until it is empty and returns the claimed IDs.
func claimOrder(t *testing.T, store *postgres.Store, ch domain.Channel) []string {
	t.Helper()
	var ids []string
	for {
		c, ok, err := claim(t.Context(), store, request(ch))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ids
		}
		ids = append(ids, c.ID)
	}
}

// TestPriorityOrder: 50 low, then 50 high on one channel. Every high is
// claimed before the first low; within a level, by available_at (R9, K8).
func TestPriorityOrder(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	// available_at falls as the insert order rises, so an order by ID would
	// differ from the order by available_at.
	for _, p := range []domain.Priority{domain.PriorityLow, domain.PriorityHigh} {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, available_at)
			 SELECT '+905551234567', 'sms', $1 || '-' || g, $1::notification_priority, 'corr',
			        clock_timestamp() - interval '1 minute' - g * interval '1 millisecond'
			 FROM generate_series(1, 50) AS g`, string(p)); err != nil {
			t.Fatal(err)
		}
	}

	ids := claimOrder(t, store, domain.ChannelSMS)
	if len(ids) != 100 {
		t.Fatalf("claimed %d notifications, want 100", len(ids))
	}
	type claimedRow struct {
		priority    string
		availableAt time.Time
	}
	rows := make([]claimedRow, len(ids))
	for i, id := range ids {
		if err := pool.QueryRow(t.Context(),
			`SELECT priority::text, available_at FROM notifications WHERE id = $1::uuid`, id).Scan(
			&rows[i].priority, &rows[i].availableAt); err != nil {
			t.Fatal(err)
		}
	}
	firstLow := slices.IndexFunc(rows, func(r claimedRow) bool { return r.priority == "low" })
	if firstLow != 50 || slices.ContainsFunc(rows[:50], func(r claimedRow) bool { return r.priority != "high" }) {
		t.Fatalf("first low claimed at position %d, want every high (positions 0–49) first", firstLow)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].priority == rows[i-1].priority && rows[i].availableAt.Before(rows[i-1].availableAt) {
			t.Fatalf("claim %d (%s) is due earlier than claim %d: not ordered by available_at", i, rows[i].priority, i-1)
		}
	}
}

// TestPriorityTieBreak: one ready row per level, a high due in the future,
// and two ready normals due at the same time. Claim order: the ready high, the
// lower-ID normal, the other normal, the low; the future high only once it is
// due (R9, K8).
func TestPriorityTieBreak(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	low := insertOn(t, pool, domain.ChannelSMS, domain.PriorityLow, "low", -20*time.Second) // oldest of all
	high := insertOn(t, pool, domain.ChannelSMS, domain.PriorityHigh, "high", -10*time.Second)
	future := insertOn(t, pool, domain.ChannelSMS, domain.PriorityHigh, "future", time.Second)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, available_at)
		 SELECT '+905551234567', 'sms', 'normal', 'normal', 'corr', clock_timestamp() - interval '5 seconds'
		 FROM generate_series(1, 2)`); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(t.Context(), `SELECT id::text FROM notifications WHERE content = 'normal' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	normals, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(normals) != 2 {
		t.Fatalf("normals %v err=%v", normals, err)
	}

	want := []string{high, normals[0], normals[1], low}
	if got := claimOrder(t, store, domain.ChannelSMS); !slices.Equal(got, want) {
		t.Fatalf("claim order %v, want %v (ready high, lower-ID normal, other normal, low)", got, want)
	}
	time.Sleep(1100 * time.Millisecond) // the future high becomes due by the database clock
	if got := claimOrder(t, store, domain.ChannelSMS); !slices.Equal(got, []string{future}) {
		t.Fatalf("after its time: claimed %v, want the future high", got)
	}
}

// TestClaimQueryPlan: with 10,000 queued rows and fresh statistics, the
// claim statement reads notifications through the partial claim index (K8).
func TestClaimQueryPlan(t *testing.T) {
	pool := testdb.New(t)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, available_at)
		 SELECT 'r', (ARRAY['sms','email','push'])[1 + g % 3], 'c',
		        (ARRAY['high','normal','low'])[1 + (g / 3) % 3]::notification_priority, 'corr',
		        clock_timestamp() - g * interval '1 millisecond'
		 FROM generate_series(1, 10000) AS g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `ANALYZE notifications`); err != nil {
		t.Fatal(err)
	}
	req := limited(domain.ChannelSMS)
	rows, err := pool.Query(t.Context(), "EXPLAIN "+postgres.ClaimSQL, string(req.Channel), req.MaxAttempts,
		req.Lease.Microseconds(), req.PermitInterval.Microseconds(), req.PermitGrace.Microseconds())
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	t.Logf("plan:\n%s", plan)
	if !strings.Contains(plan, "notifications_claim_idx") {
		t.Fatal("the claim plan does not use notifications_claim_idx")
	}
}

// TestMissedStartGivesBack: the give-back after a missed start deadline
// changes the row once, only under a valid lease and the matching token, and
// never moves the permit back (K42).
func TestMissedStartGivesBack(t *testing.T) {
	setup := func(t *testing.T) (*pgxpool.Pool, *postgres.Store, string) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		return pool, store, insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "s", -time.Second)
	}

	t.Run("valid token, then a second time", func(t *testing.T) {
		pool, store, id := setup(t)
		res, err := store.ClaimNext(t.Context(), limited(domain.ChannelSMS))
		if err != nil || res.Outcome != domain.ClaimClaimed {
			t.Fatalf("claim: %+v err=%v", res, err)
		}
		permit := nextPermit(t, pool, domain.ChannelSMS)
		given, err := store.GiveBack(t.Context(), domain.AttemptOf(res.Claim))
		if err != nil || !given {
			t.Fatalf("give back: %v err=%v, want one row changed", given, err)
		}
		r := readLease(t, pool, id)
		if r.status != "queued" || r.attempts != 0 || r.token != nil || r.expires != nil {
			t.Fatalf("row %+v, want queued with attempts 0 and no lease", r)
		}
		if after := nextPermit(t, pool, domain.ChannelSMS); !after.Equal(permit) {
			t.Fatalf("give-back moved next_permit_at from %s to %s", permit, after)
		}
		given, err = store.GiveBack(t.Context(), domain.AttemptOf(res.Claim))
		if err != nil || given {
			t.Fatalf("second give back: %v err=%v, want 0 rows", given, err)
		}
		if r := readLease(t, pool, id); r.attempts != 0 {
			t.Fatalf("attempts %d after the second give-back, want 0", r.attempts)
		}
	})

	t.Run("expired lease", func(t *testing.T) {
		pool, store, id := setup(t)
		c := mustClaim(t, store)
		expire(t, pool, id)
		if given, err := store.GiveBack(t.Context(), domain.AttemptOf(c)); err != nil || given {
			t.Fatalf("give back: %v err=%v, want 0 rows", given, err)
		}
		if r := readLease(t, pool, id); r.status != "processing" || r.attempts != 1 {
			t.Fatalf("row %+v, want processing with attempts 1", r)
		}
	})

	t.Run("recovered and claimed again", func(t *testing.T) {
		pool, store, id := setup(t)
		a := mustClaim(t, store)
		expire(t, pool, id)
		recoverAll(t, store)
		b := mustClaim(t, store)
		if given, err := store.GiveBack(t.Context(), domain.AttemptOf(a)); err != nil || given {
			t.Fatalf("give back with the old token: %v err=%v, want 0 rows", given, err)
		}
		if r := readLease(t, pool, id); r.attempts != 2 || r.token == nil || *r.token != b.LeaseToken {
			t.Fatalf("row %+v, want B's lease with attempts 2", r)
		}
	})
}
