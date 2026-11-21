package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

// begin opens a transaction the test holds and commits itself; it is rolled
// back at the end of the test if still open.
func begin(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// waitBlocked waits until a session of the test database waits on a lock:
// the step the test started is now queued behind the transaction it holds.
func waitBlocked(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session waited on a lock within 5 seconds")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// claimOn runs the claim statement on tx and returns the claimed ID and
// lease token, or "" when it claimed nothing.
func claimOn(t *testing.T, tx pgx.Tx) (id, token string) {
	t.Helper()
	req := request(domain.ChannelSMS)
	var (
		generation, openUS, waitUS                   *int64
		hasPermit                                    bool
		gotID, recipient, channel, content, corr, tk *string
		attempt                                      *int
		leaseUS, permitUS                            *int64
		traceparent                                  *string
	)
	err := tx.QueryRow(t.Context(), postgres.ClaimSQL, string(req.Channel), req.MaxAttempts, req.Lease.Microseconds(),
		req.PermitInterval.Microseconds(), req.PermitGrace.Microseconds()).Scan(
		&generation, &openUS, &hasPermit, &waitUS,
		&gotID, &recipient, &channel, &content, &corr, &attempt, &tk, &leaseUS, &permitUS, &traceparent)
	if err != nil {
		t.Fatalf("claim on the held transaction: %v", err)
	}
	if gotID == nil {
		return "", ""
	}
	return *gotID, *tk
}

type cancelResult struct {
	n   domain.Notification
	err error
}

func conflictStatus(err error) domain.Status {
	var c *domain.CancelConflictError
	if errors.As(err, &c) {
		return c.Status
	}
	return ""
}

// TestCancelClaimRace: cancel and claim race for one queued row; whichever
// locks it first wins (R5, K9).
func TestCancelClaimRace(t *testing.T) {
	t.Run("cancel first", func(t *testing.T) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		id := insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "c", -time.Second)

		a := begin(t, pool)
		n, err := postgres.CancelIn(t.Context(), a, id)
		if err != nil || n.Status != domain.StatusCancelled {
			t.Fatalf("cancel on A: %+v err=%v", n, err)
		}
		res, err := store.ClaimNext(t.Context(), request(domain.ChannelSMS))
		if err != nil || res.Outcome != domain.ClaimEmpty {
			t.Fatalf("claim on B while A holds the cancel: %+v err=%v, want no notification", res, err)
		}
		if err := a.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if r := readLease(t, pool, id); r.status != "cancelled" || r.attempts != 0 || r.token != nil {
			t.Fatalf("row %+v, want cancelled with attempts 0", r)
		}
	})

	t.Run("claim first", func(t *testing.T) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		id := insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "c", -time.Second)

		b := begin(t, pool)
		claimed, token := claimOn(t, b)
		if claimed != id {
			t.Fatalf("B claimed %q, want %s", claimed, id)
		}
		done := make(chan cancelResult, 1)
		go func() {
			n, err := store.CancelNotification(context.Background(), id)
			done <- cancelResult{n, err}
		}()
		waitBlocked(t, pool)
		if err := b.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if conflictStatus(got.err) != domain.StatusProcessing {
			t.Fatalf("cancel after B's claim: %+v err=%v, want a conflict with processing", got.n, got.err)
		}
		if r := readLease(t, pool, id); r.status != "processing" || r.token == nil || *r.token != token {
			t.Fatalf("row %+v, want processing under B's token", r)
		}
	})
}

// TestCancelResultRace: cancel and a worker's processing → queued write race
// for a claimed row (R5, K9).
func TestCancelResultRace(t *testing.T) {
	setup := func(t *testing.T) (*pgxpool.Pool, *postgres.Store, string, string) {
		pool := testdb.New(t)
		store := postgres.NewStore(pool)
		id := insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "c", -time.Second)
		c := mustClaim(t, store)
		return pool, store, id, c.LeaseToken
	}

	t.Run("result first", func(t *testing.T) {
		pool, store, id, token := setup(t)
		w := begin(t, pool)
		tag, err := w.Exec(t.Context(), postgres.MarkRetrySQL, id, token, "temporary: status 503", int64(0))
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("retry write on W: %v rows=%d", err, tag.RowsAffected())
		}
		done := make(chan cancelResult, 1)
		go func() {
			n, err := store.CancelNotification(context.Background(), id)
			done <- cancelResult{n, err}
		}()
		waitBlocked(t, pool)
		if err := w.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.err != nil || got.n.Status != domain.StatusCancelled {
			t.Fatalf("cancel after the retry write: %+v err=%v, want cancelled", got.n, got.err)
		}
		if r := readLease(t, pool, id); r.status != "cancelled" || r.token != nil || r.expires != nil {
			t.Fatalf("row %+v, want cancelled with the lease fields empty", r)
		}
	})

	t.Run("cancel first", func(t *testing.T) {
		pool, store, id, token := setup(t)
		a := begin(t, pool)
		_, err := postgres.CancelIn(t.Context(), a, id)
		if conflictStatus(err) != domain.StatusProcessing {
			t.Fatalf("cancel on A: err=%v, want a conflict with processing", err)
		}
		done := make(chan error, 1)
		var changed bool
		go func() {
			var err error
			changed, err = store.MarkRetry(context.Background(), id, token, "temporary: status 503", 0)
			done <- err
		}()
		waitBlocked(t, pool)
		if err := a.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil || !changed {
			t.Fatalf("retry write after the cancel: changed=%v err=%v, want one row", changed, err)
		}
		if r := readLease(t, pool, id); r.status != "queued" || r.token != nil {
			t.Fatalf("row %+v, want queued again, so it is retried", r)
		}
	})
}

// TestCancelClaimRaceRepeated: 200 runs, each with a fresh queued row, start
// a cancel and a claim at the same moment. Every run ends with exactly one
// winner: cancelled and no claim, or claimed and a 409 (R5, K9).
func TestCancelClaimRaceRepeated(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	other := postgres.NewStore(secondPoolFor(t, pool))
	const runs = 200
	var cancelWon, claimWon int
	for i := range runs {
		id := insertOn(t, pool, domain.ChannelSMS, domain.PriorityNormal, "race", -time.Second)
		start := make(chan struct{})
		cancelled := make(chan cancelResult, 1)
		claimed := make(chan domain.ClaimResult, 1)
		go func() {
			<-start
			n, err := store.CancelNotification(context.Background(), id)
			cancelled <- cancelResult{n, err}
		}()
		go func() {
			<-start
			res, err := other.ClaimNext(context.Background(), request(domain.ChannelSMS))
			if err != nil {
				res.Outcome = "error: " + domain.ClaimOutcome(err.Error())
			}
			claimed <- res
		}()
		close(start)
		c, cl := <-cancelled, <-claimed

		switch {
		case c.err == nil && c.n.Status == domain.StatusCancelled && cl.Outcome == domain.ClaimEmpty:
			cancelWon++
		case conflictStatus(c.err) == domain.StatusProcessing && cl.Outcome == domain.ClaimClaimed && cl.Claim.ID == id:
			claimWon++
		default:
			t.Fatalf("run %d: cancel %+v err=%v, claim %+v; want exactly one winner", i, c.n.Status, c.err, cl)
		}
	}
	t.Logf("%d runs: cancel won %d, claim won %d", runs, cancelWon, claimWon)
}

// secondPoolFor opens another pool on the test database, so the two sides of
// a race use separate connections.
func secondPoolFor(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	other, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return other
}
