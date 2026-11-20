package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// Breaker writes (K34). Each one reads the database clock once (c.now) and
// returns the breaker as it left it: generation, cooldown and how long it
// stays open (null while closed), in microseconds.
const (
	breakerReturning = `
		RETURNING b.generation,
		          floor(extract(epoch FROM b.cooldown) * 1000000)::bigint,
		          floor(extract(epoch FROM b.open_until - c.now) * 1000000)::bigint`

	// resetFailuresSQL: a success of the current generation while closed
	// clears the failure count. A row that does not match is not locked, so
	// successes do not wait on each other while the count is already zero.
	resetFailuresSQL = `
		UPDATE provider_breaker
		SET consecutive_failures = 0
		WHERE generation = $1 AND open_until IS NULL AND consecutive_failures > 0`

	// countFailureSQL: an outage failure of the current generation while
	// closed counts; reaching the threshold ($2) opens the breaker for the
	// first cooldown ($3), in a new generation with the count back at zero.
	countFailureSQL = `
		UPDATE provider_breaker AS b
		SET consecutive_failures = CASE WHEN b.consecutive_failures + 1 >= $2 THEN 0
		                                ELSE b.consecutive_failures + 1 END,
		    generation           = CASE WHEN b.consecutive_failures + 1 >= $2 THEN b.generation + 1
		                                ELSE b.generation END,
		    open_until           = CASE WHEN b.consecutive_failures + 1 >= $2
		                                THEN c.now + $3::bigint * interval '1 microsecond' END,
		    cooldown             = CASE WHEN b.consecutive_failures + 1 >= $2
		                                THEN $3::bigint * interval '1 microsecond' ELSE b.cooldown END
		FROM (SELECT clock_timestamp() AS now) AS c
		WHERE b.generation = $1 AND b.open_until IS NULL` + breakerReturning

	// quotaOpenSQL: a 429 of the current generation, under a profile that
	// states a shared quota, opens the closed breaker until its Retry-After
	// ($2) has passed. The cooldown starts again from the first value ($3).
	quotaOpenSQL = `
		UPDATE provider_breaker AS b
		SET generation           = b.generation + 1,
		    consecutive_failures = 0,
		    open_until           = c.now + $2::bigint * interval '1 microsecond',
		    cooldown             = $3::bigint * interval '1 microsecond'
		FROM (SELECT clock_timestamp() AS now) AS c
		WHERE b.generation = $1 AND b.open_until IS NULL` + breakerReturning

	// lockBreakerSQL locks the breaker row before any other row of a probe's
	// result write (K34's lock order). authority is true while the slot holds
	// this probe's token ($1) and its probe lease is valid by the database
	// clock; holds is true while the slot holds the token at all.
	lockBreakerSQL = `
		SELECT b.generation,
		       coalesce(b.probe_token = $1::text::uuid AND b.probe_expires_at > clock_timestamp(), false),
		       coalesce(b.probe_token = $1::text::uuid, false)
		FROM provider_breaker AS b
		FOR UPDATE`

	// closeSQL: the probe's 202 closes the breaker, clears the count, sets the
	// cooldown back to the first value ($1) and frees the slot.
	closeSQL = `
		UPDATE provider_breaker AS b
		SET open_until           = NULL,
		    consecutive_failures = 0,
		    cooldown             = $1::bigint * interval '1 microsecond',
		    probe_token          = NULL,
		    probe_expires_at     = NULL
		FROM (SELECT clock_timestamp() AS now) AS c
		WHERE b.id` + breakerReturning

	// reopenSQL: the probe's outage failure reopens the breaker in a new
	// generation with the cooldown doubled, at most the maximum ($1), and
	// frees the slot.
	reopenSQL = `
		UPDATE provider_breaker AS b
		SET generation           = b.generation + 1,
		    consecutive_failures = 0,
		    cooldown             = least(b.cooldown * 2, $1::bigint * interval '1 microsecond'),
		    open_until           = c.now + least(b.cooldown * 2, $1::bigint * interval '1 microsecond'),
		    probe_token          = NULL,
		    probe_expires_at     = NULL
		FROM (SELECT clock_timestamp() AS now) AS c
		WHERE b.id` + breakerReturning

	// quotaReopenSQL: the probe's 429 under a shared quota reopens the
	// breaker until its Retry-After ($1) has passed; a pause already set to
	// end later is kept. The cooldown stays as it is.
	quotaReopenSQL = `
		UPDATE provider_breaker AS b
		SET generation           = b.generation + 1,
		    consecutive_failures = 0,
		    open_until           = greatest(b.open_until, c.now + $1::bigint * interval '1 microsecond'),
		    probe_token          = NULL,
		    probe_expires_at     = NULL
		FROM (SELECT clock_timestamp() AS now) AS c
		WHERE b.id` + breakerReturning

	// freeSlotSQL frees the slot while it still holds the probe's token ($1);
	// the generation, the count and the cooldown stay as they are.
	freeSlotSQL = `
		UPDATE provider_breaker
		SET probe_token = NULL, probe_expires_at = NULL
		WHERE probe_token = $1::text::uuid`
)

// execer runs one statement on the pool or on a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryRower runs one statement that returns one row.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Finish writes a delivery's result to its notification under the claim's
// lease (K3) and applies the circuit breaker's rules to it (K34). A result
// whose lease is no longer valid changes neither the notification nor the
// breaker; a result of an older generation changes only the notification.
//
// A regular claim's notification write commits on its own, and the breaker
// write, when the result can move the breaker, follows in its own statement:
// no transaction holds both rows. A probe's result is written in one
// transaction that locks the breaker row first.
func (s *Store) Finish(ctx context.Context, f domain.Finish) (domain.Finished, error) {
	if f.Attempt.Probe {
		return s.finishProbe(ctx, f)
	}
	written, err := writeResult(ctx, s.pool, f.Attempt, f.Result)
	if err != nil || !written {
		return domain.Finished{Written: written}, err
	}
	gen := f.Attempt.Generation
	switch {
	case f.Result.QuotaPause > 0:
		return breakerWrite(ctx, s.pool, domain.BreakerOpened, "open breaker",
			quotaOpenSQL, gen, f.Result.QuotaPause.Microseconds(), f.Policy.Cooldown.Microseconds())
	case f.Result.Outage:
		fin, err := breakerWrite(ctx, s.pool, domain.BreakerOpened, "count failure",
			countFailureSQL, gen, f.Policy.Threshold, f.Policy.Cooldown.Microseconds())
		if fin.OpenFor <= 0 { // counted, still closed
			fin = domain.Finished{Written: true}
		}
		return fin, err
	case f.Result.Kind == domain.ResultSent:
		if _, err := s.pool.Exec(ctx, resetFailuresSQL, gen); err != nil {
			return domain.Finished{Written: true}, fmt.Errorf("reset failures: %w", err)
		}
	}
	return domain.Finished{Written: true}, nil
}

// finishProbe writes a probe's result. Only a probe that still holds the
// slot under a valid probe lease, in the generation it was taken, moves the
// breaker: a 202 closes it, an outage failure or a shared-quota 429 reopens
// it. Every other outcome, and every stale probe still holding the slot,
// frees the slot without touching the generation, the count or the cooldown.
func (s *Store) finishProbe(ctx context.Context, f domain.Finish) (domain.Finished, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Finished{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	var (
		gen              int64
		authority, holds bool
	)
	if err := tx.QueryRow(ctx, lockBreakerSQL, f.Attempt.LeaseToken).Scan(&gen, &authority, &holds); err != nil {
		return domain.Finished{}, fmt.Errorf("lock breaker: %w", err)
	}
	written, err := writeResult(ctx, tx, f.Attempt, f.Result)
	if err != nil {
		return domain.Finished{}, err
	}

	fin := domain.Finished{Written: written}
	moves := written && authority && gen == f.Attempt.Generation
	switch {
	case moves && f.Result.Kind == domain.ResultSent:
		fin, err = breakerWrite(ctx, tx, domain.BreakerClosed, "close breaker", closeSQL, f.Policy.Cooldown.Microseconds())
	case moves && f.Result.Outage:
		fin, err = breakerWrite(ctx, tx, domain.BreakerReopened, "reopen breaker", reopenSQL, f.Policy.MaxCooldown.Microseconds())
	case moves && f.Result.QuotaPause > 0:
		fin, err = breakerWrite(ctx, tx, domain.BreakerReopened, "reopen breaker", quotaReopenSQL, f.Result.QuotaPause.Microseconds())
	case holds:
		if _, err = tx.Exec(ctx, freeSlotSQL, f.Attempt.LeaseToken); err != nil {
			err = fmt.Errorf("free probe slot: %w", err)
		}
	}
	if err != nil {
		return domain.Finished{}, err
	}
	fin.Written = written
	if err := tx.Commit(ctx); err != nil {
		return domain.Finished{}, fmt.Errorf("commit: %w", err)
	}
	return fin, nil
}

// GiveBack returns a claim whose send never started (K42): queued again with
// its attempt returned. It reports false when the lease was no longer valid
// or the claim was already given back, in which case no row changed. A
// probe's give-back also frees the slot while it holds the probe's token, in
// one transaction that locks the breaker row first (K34).
func (s *Store) GiveBack(ctx context.Context, a domain.Attempt) (bool, error) {
	if !a.Probe {
		return execOne(ctx, s.pool, "give back", giveBackSQL, a.ID, a.LeaseToken)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		gen              int64
		authority, holds bool
	)
	if err := tx.QueryRow(ctx, lockBreakerSQL, a.LeaseToken).Scan(&gen, &authority, &holds); err != nil {
		return false, fmt.Errorf("lock breaker: %w", err)
	}
	given, err := execOne(ctx, tx, "give back", giveBackSQL, a.ID, a.LeaseToken)
	if err != nil {
		return false, err
	}
	if holds {
		if _, err := tx.Exec(ctx, freeSlotSQL, a.LeaseToken); err != nil {
			return false, fmt.Errorf("free probe slot: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return given, nil
}

// writeResult writes r to the notification under a's lease (K3).
func writeResult(ctx context.Context, db execer, a domain.Attempt, r domain.Result) (bool, error) {
	switch r.Kind {
	case domain.ResultSent:
		var messageID *string
		if r.Acceptance.MessageID != "" {
			messageID = &r.Acceptance.MessageID
		}
		var timestamp *time.Time
		if !r.Acceptance.Timestamp.IsZero() {
			timestamp = &r.Acceptance.Timestamp
		}
		return execOne(ctx, db, "mark sent", markSentSQL, a.ID, a.LeaseToken, messageID, timestamp)
	case domain.ResultRetry:
		return execOne(ctx, db, "mark retry", markRetrySQL, a.ID, a.LeaseToken, r.LastError, r.Wait.Microseconds())
	case domain.ResultFailed:
		return execOne(ctx, db, "mark failed", markFailedSQL, a.ID, a.LeaseToken, r.LastError)
	default:
		return false, fmt.Errorf("unknown result kind %q", r.Kind)
	}
}

// breakerWrite runs one breaker write and reports change when it matched the
// row; a write that matched nothing leaves the breaker unchanged.
func breakerWrite(ctx context.Context, db queryRower, change domain.BreakerChange, what, query string, args ...any) (
	domain.Finished, error,
) {
	var (
		gen, cooldownUS int64
		openUS          *int64
	)
	err := db.QueryRow(ctx, query, args...).Scan(&gen, &cooldownUS, &openUS)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Finished{Written: true}, nil
	case err != nil:
		return domain.Finished{Written: true}, fmt.Errorf("%s: %w", what, err)
	}
	fin := domain.Finished{Written: true, Breaker: change, Generation: gen, Cooldown: microseconds(cooldownUS)}
	if openUS != nil {
		fin.OpenFor = microseconds(*openUS)
	}
	return fin, nil
}

func microseconds(us int64) time.Duration {
	return time.Duration(us) * time.Microsecond
}

func execOne(ctx context.Context, db execer, what, query string, args ...any) (bool, error) {
	tag, err := db.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return tag.RowsAffected() == 1, nil
}
