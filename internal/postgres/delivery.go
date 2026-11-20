package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

const (
	// claimSQL is one claim attempt on one channel, in one statement and so
	// in one short transaction (K5, K8, K42). $1 channel, $2 max attempts,
	// $3 lease, $4 permit interval, $5 permit grace (microseconds).
	//
	//  1. permit locks the channel's limiter row, skipping it when another
	//     worker holds it (no row: busy), and reads the database clock once:
	//     the permit time. MATERIALIZED keeps it one reading.
	//  2. next takes one due notification of that channel, only when the
	//     permit is due (otherwise the scalar subquery is NULL and nothing
	//     matches): high → normal → low as the enum declares them, then
	//     available_at, then id, served by notifications_claim_idx.
	//  3. claimed writes the lease and counts the attempt (K3, K9).
	//  4. moved sets the next permit PERMIT_INTERVAL after this one, only when
	//     a notification was claimed: an empty channel keeps its permit.
	//
	// The lease, the next permit and the start deadline all derive from the
	// one permit time. The time left until the lease's end and until the start
	// deadline is read at the end of the statement; a later reading can only
	// shorten it. wait_us is how long until the permit is due; it is above
	// zero only when the attempt was throttled.
	claimSQL = `
		WITH permit AS MATERIALIZED (
			SELECT l.channel, l.next_permit_at, c.permit_at
			FROM channel_limits AS l, (SELECT clock_timestamp() AS permit_at) AS c
			WHERE l.channel = $1
			FOR UPDATE OF l SKIP LOCKED
		), next AS (
			SELECT n.id
			FROM notifications AS n
			WHERE n.channel = $1
			  AND n.status = 'queued'
			  AND n.available_at <= (SELECT p.permit_at FROM permit AS p WHERE p.next_permit_at <= p.permit_at)
			  AND n.attempts < $2
			ORDER BY n.priority, n.available_at, n.id
			LIMIT 1
			FOR UPDATE OF n SKIP LOCKED
		), claimed AS (
			UPDATE notifications AS n
			SET status           = 'processing',
			    lease_token      = gen_random_uuid(),
			    lease_expires_at = p.permit_at + $3::bigint * interval '1 microsecond',
			    attempts         = n.attempts + 1,
			    updated_at       = p.permit_at
			FROM next, permit AS p
			WHERE n.id = next.id
			RETURNING n.id, n.recipient, n.channel, n.content, n.correlation_id, n.attempts,
			          n.lease_token, n.lease_expires_at
		), moved AS (
			UPDATE channel_limits AS l
			SET next_permit_at = p.permit_at + $4::bigint * interval '1 microsecond'
			FROM permit AS p
			WHERE l.channel = p.channel AND EXISTS (SELECT 1 FROM claimed)
		)
		SELECT floor(extract(epoch FROM p.next_permit_at - p.permit_at) * 1000000)::bigint,
		       c.id::text, c.recipient, c.channel, c.content, c.correlation_id, c.attempts,
		       c.lease_token::text,
		       floor(extract(epoch FROM c.lease_expires_at - clock_timestamp()) * 1000000)::bigint,
		       floor(extract(epoch FROM p.permit_at + $5::bigint * interval '1 microsecond' - clock_timestamp())
		             * 1000000)::bigint
		FROM permit AS p LEFT JOIN claimed AS c ON true`

	// Every result write reads the database clock once (c.now) and carries the
	// lease condition (K3, K9): the row is still processing, under this claim's
	// token, and the lease has not expired by the database clock. Each write
	// also clears both lease columns.
	resultFrom = `
		FROM (SELECT clock_timestamp() AS now) AS c
		WHERE n.id = $1::text::uuid
		  AND n.status = 'processing'
		  AND n.lease_token = $2::text::uuid
		  AND n.lease_expires_at > c.now`

	markSentSQL = `
		UPDATE notifications AS n
		SET status              = 'sent',
		    sent_at             = c.now,
		    completed_at        = c.now,
		    provider_message_id = $3,
		    provider_timestamp  = $4,
		    lease_token         = NULL,
		    lease_expires_at    = NULL,
		    updated_at          = c.now` + resultFrom

	markFailedSQL = `
		UPDATE notifications AS n
		SET status           = 'failed',
		    last_error       = $3,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    completed_at     = c.now,
		    updated_at       = c.now` + resultFrom

	// markRetrySQL puts a temporary failure back in the queue; the wait is
	// added to the database clock, so a waiting retry holds no worker and
	// survives a crash (K7).
	markRetrySQL = `
		UPDATE notifications AS n
		SET status           = 'queued',
		    available_at     = c.now + $4::bigint * interval '1 microsecond',
		    last_error       = $3,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = c.now` + resultFrom

	// giveBackSQL undoes a claim whose send never started because its permit's
	// start deadline had passed (K42): the request certainly never reached the
	// HTTP transport, so the attempt is returned. It carries the lease
	// condition and clears the token, so it can change the row only once. The
	// permit is not given back.
	giveBackSQL = `
		UPDATE notifications AS n
		SET status           = 'queued',
		    attempts         = n.attempts - 1,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = c.now` + resultFrom

	// recoverSQL takes back at most $2 rows whose lease has expired by the
	// database clock (K3, K9, K40). SKIP LOCKED lets several workers recover at
	// the same time without taking the same row. A row with attempts left goes
	// back to queued; one without fails. It changes statuses only: no permit,
	// no circuit breaker.
	recoverSQL = `
		WITH expired AS (
			SELECT id
			FROM notifications
			WHERE status = 'processing' AND lease_expires_at <= clock_timestamp()
			ORDER BY lease_expires_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notifications AS n
		SET status           = CASE WHEN n.attempts < $1 THEN 'queued' ELSE 'failed' END::notification_status,
		    last_error       = $3,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    completed_at     = CASE WHEN n.attempts < $1 THEN NULL ELSE c.now END,
		    updated_at       = c.now
		FROM expired, (SELECT clock_timestamp() AS now) AS c
		WHERE n.id = expired.id
		RETURNING n.id::text, n.correlation_id, n.status::text, n.attempts`
)

// RecoveredError is the last_error recovery writes.
const RecoveredError = "temporary: lease expired before a result was written"

// ClaimNext makes one claim attempt on req.Channel (K5). It takes the
// channel's permit and one due notification together, or neither.
func (s *Store) ClaimNext(ctx context.Context, req domain.ClaimRequest) (domain.ClaimResult, error) {
	var (
		waitUS                                       int64
		id, recipient, channel, content, corr, token *string
		attempt                                      *int
		leaseUS, permitUS                            *int64
	)
	err := s.pool.QueryRow(ctx, claimSQL, string(req.Channel), req.MaxAttempts, req.Lease.Microseconds(),
		req.PermitInterval.Microseconds(), req.PermitGrace.Microseconds()).Scan(
		&waitUS, &id, &recipient, &channel, &content, &corr, &attempt, &token, &leaseUS, &permitUS)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// SKIP LOCKED found the limiter row held by another worker.
		return domain.ClaimResult{Outcome: domain.ClaimBusy}, nil
	case err != nil:
		return domain.ClaimResult{}, fmt.Errorf("claim: %w", err)
	case id != nil:
		return domain.ClaimResult{Outcome: domain.ClaimClaimed, Claim: domain.Claim{
			ID:              *id,
			Recipient:       *recipient,
			Channel:         domain.Channel(*channel),
			Content:         *content,
			CorrelationID:   *corr,
			Attempt:         *attempt,
			LeaseToken:      *token,
			LeaseRemaining:  time.Duration(*leaseUS) * time.Microsecond,
			PermitRemaining: time.Duration(*permitUS) * time.Microsecond,
		}}, nil
	case waitUS > 0:
		return domain.ClaimResult{Outcome: domain.ClaimThrottled, Wait: time.Duration(waitUS) * time.Microsecond}, nil
	default:
		return domain.ClaimResult{Outcome: domain.ClaimEmpty}, nil
	}
}

// MarkSent records the provider's acceptance. It reports false when the lease
// was no longer valid, in which case no row changed.
func (s *Store) MarkSent(ctx context.Context, id, leaseToken string, acc domain.Acceptance) (bool, error) {
	var messageID *string
	if acc.MessageID != "" {
		messageID = &acc.MessageID
	}
	var timestamp *time.Time
	if !acc.Timestamp.IsZero() {
		timestamp = &acc.Timestamp
	}
	return s.exec(ctx, "mark sent", markSentSQL, id, leaseToken, messageID, timestamp)
}

// MarkFailed ends the notification as failed with lastError. It reports false
// when the lease was no longer valid, in which case no row changed.
func (s *Store) MarkFailed(ctx context.Context, id, leaseToken, lastError string) (bool, error) {
	return s.exec(ctx, "mark failed", markFailedSQL, id, leaseToken, lastError)
}

// MarkRetry puts the notification back to queued, due wait after the database
// clock, with lastError. It reports false when the lease was no longer valid,
// in which case no row changed.
func (s *Store) MarkRetry(ctx context.Context, id, leaseToken, lastError string, wait time.Duration) (bool, error) {
	return s.exec(ctx, "mark retry", markRetrySQL, id, leaseToken, lastError, wait.Microseconds())
}

// GiveBack returns a claim whose send never started (K42): queued again with
// its attempt returned. It reports false when the lease was no longer valid
// or the claim was already given back, in which case no row changed.
func (s *Store) GiveBack(ctx context.Context, id, leaseToken string) (bool, error) {
	return s.exec(ctx, "give back", giveBackSQL, id, leaseToken)
}

func (s *Store) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecoverExpired takes back at most batch notifications whose lease expired:
// queued while they have had fewer than maxAttempts attempts, failed otherwise.
func (s *Store) RecoverExpired(ctx context.Context, maxAttempts, batch int) ([]domain.Recovered, error) {
	rows, err := s.pool.Query(ctx, recoverSQL, maxAttempts, batch, RecoveredError)
	if err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Recovered, error) {
		var (
			r      domain.Recovered
			status string
		)
		err := row.Scan(&r.ID, &r.CorrelationID, &status, &r.Attempts)
		r.Status = domain.Status(status)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	return out, nil
}
